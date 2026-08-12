// Package cli implements the unring command line.
package cli

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime/debug"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/charmbracelet/x/ansi"
	"github.com/hyj28/unring/internal/adapter"
	"github.com/hyj28/unring/internal/audit"
	"github.com/hyj28/unring/internal/childenv"
	"github.com/hyj28/unring/internal/ghshim"
	"github.com/hyj28/unring/internal/httpsproxy"
	"github.com/hyj28/unring/internal/localrollback"
	"github.com/hyj28/unring/internal/pgproxy"
	"github.com/hyj28/unring/internal/runner"
	"github.com/jackc/pgx/v5/pgconn"
	"golang.org/x/term"
)

const (
	internalErrorExitCode = 1
	usageExitCode         = 2
	defaultReviewWidth    = 80

	quietSessionDisclosure = "unring: nothing intercepted. Outbound is not covered unless --outbound was given. Not visible to unring: SSH/git push, raw sockets, unshimmed CLIs."
	homeScanExclusions     = "Library, node_modules, .git, .cache, and go/pkg"
)

type stringListFlag []string

func (values *stringListFlag) String() string { return strings.Join(*values, ",") }

func (values *stringListFlag) Set(value string) error {
	if strings.TrimSpace(value) == "" {
		return errors.New("watched path cannot be empty")
	}
	*values = append(*values, value)
	return nil
}

type postgresSession interface {
	Address() string
	Done() <-chan struct{}
	Err() error
	Summary() pgproxy.Summary
	Seal(context.Context) error
	Finalize(context.Context, pgproxy.Decision) error
	Close() error
}

type unconfiguredPostgresSession struct {
	done chan struct{}
}

func newUnconfiguredPostgresSession() *unconfiguredPostgresSession {
	return &unconfiguredPostgresSession{done: make(chan struct{})}
}

func (session *unconfiguredPostgresSession) Address() string {
	return ""
}

func (session *unconfiguredPostgresSession) Done() <-chan struct{} {
	return session.done
}

func (session *unconfiguredPostgresSession) Err() error {
	return nil
}

func (session *unconfiguredPostgresSession) Summary() pgproxy.Summary {
	return pgproxy.Summary{
		InterceptionStatus: pgproxy.InterceptionNotConfigured,
		FullyReversible:    true,
		Changes:            pgproxy.ChangeSummary{Complete: true},
		Sealed:             true,
	}
}

func (session *unconfiguredPostgresSession) Seal(context.Context) error {
	return nil
}

func (session *unconfiguredPostgresSession) Finalize(context.Context, pgproxy.Decision) error {
	return nil
}

func (session *unconfiguredPostgresSession) Close() error {
	return nil
}

// Main runs the CLI and returns the desired process exit code.
func Main(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		printUsage(stderr)
		return usageExitCode
	}
	switch args[0] {
	case "run":
		return runCommand(args[1:], stdin, stdout, stderr)
	case "log":
		return logCommand(args[1:], stdout, stderr)
	case "restore":
		return restoreCommand(args[1:], stdout, stderr)
	case "snapshots":
		return snapshotsCommand(args[1:], stdout, stderr)
	case "--outbound":
		if len(args) == 1 {
			fmt.Fprintln(stderr, "unring: no child command given")
			return usageExitCode
		}
		runArgs := append([]string{"--outbound", "--", args[1]}, args[2:]...)
		return runCommand(runArgs, stdin, stdout, stderr)
	case "help", "-h", "--help":
		printUsage(stdout)
		return 0
	case "version", "-v", "--version":
		fmt.Fprintln(stdout, versionString())
		return 0
	default:
		if isNamedAlias(args[0]) || resolvesOnPath(args[0]) {
			runArgs := append([]string{"--", args[0]}, args[1:]...)
			return runCommand(runArgs, stdin, stdout, stderr)
		}
		fmt.Fprintf(stderr, "unring: unknown command %q\n\n", args[0])
		printUsage(stderr)
		return usageExitCode
	}
}

func runCommand(args []string, stdin io.Reader, stdout, stderr io.Writer) (exitCode int) {
	flags := flag.NewFlagSet("unring run", flag.ContinueOnError)
	flags.SetOutput(stderr)
	forceCommit := flags.Bool("commit", false, "commit without prompting")
	forceDiscard := flags.Bool("discard", false, "discard without prompting")
	outboundEnabled := flags.Bool("outbound", false, "enable HTTPS interception, adapters, and the gh shim")
	retentionCap := flags.Int64("snapshot-cap-bytes", -1, "snapshot retention cap in bytes")
	var watched stringListFlag
	var watchedOnly stringListFlag
	flags.Var(&watched, "watch", "additional path to snapshot (repeatable)")
	flags.Var(&watchedOnly, "watch-only", "replacement snapshot path (repeatable; excludes the default and config watch scope)")
	flags.Usage = func() {
		fmt.Fprintln(stderr, "Usage: unring run [--commit | --discard] [--outbound] [--watch path | --watch-only path] -- <command> [args...]")
		fmt.Fprintln(stderr)
		fmt.Fprintln(stderr,
			"Run one bounded command with optional PostgreSQL and supported HTTPS effects held for review.")
		fmt.Fprintln(stderr, "Options:")
		flags.PrintDefaults()
	}
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return usageExitCode
	}
	if *forceCommit && *forceDiscard {
		fmt.Fprintln(stderr, "unring: --commit and --discard are mutually exclusive")
		return usageExitCode
	}
	if len(watched) > 0 && len(watchedOnly) > 0 {
		fmt.Fprintln(stderr, "unring: --watch and --watch-only are mutually exclusive")
		return usageExitCode
	}
	command := flags.Args()
	if len(command) == 0 {
		fmt.Fprintln(stderr, "unring: no child command given")
		flags.Usage()
		return usageExitCode
	}
	capBytes := *retentionCap
	if capBytes < -1 {
		fmt.Fprintln(stderr, "unring: --snapshot-cap-bytes must be non-negative")
		return usageExitCode
	}
	auditStore, err := audit.OpenStore()
	if err != nil {
		fmt.Fprintf(stderr, "unring: open audit log: %v\n", err)
		return internalErrorExitCode
	}
	workingDirectory, err := os.Getwd()
	if err != nil {
		fmt.Fprintf(stderr, "unring: find working directory for file snapshot: %v\n", err)
		return internalErrorExitCode
	}
	auditSession, err := auditStore.Begin(command, time.Now())
	if err != nil {
		fmt.Fprintf(stderr, "unring: begin audit log: %v\n", err)
		return internalErrorExitCode
	}
	auditRecord := auditSession.Snapshot()
	scope, err := localrollback.ResolveScope(localrollback.ScopeOptions{
		StateDir: auditStore.StateDir(), WorkingDirectory: workingDirectory,
		Watch: watched, WatchOnly: watchedOnly,
	})
	if err != nil {
		exitCode := internalErrorExitCode
		if localrollback.IsScopeConfigError(err) {
			exitCode = usageExitCode
		}
		failures := localrollback.ScopePreflightFailures(err)
		watchedPaths := make([]string, 0, len(failures))
		for _, failure := range failures {
			watchedPaths = append(watchedPaths, failure.Path)
		}
		if updateErr := auditSession.Update(func(record *audit.Record) {
			record.Files = localrollback.Summary{
				Watched: watchedPaths, Uncaptured: failures, Complete: false,
				Error: err.Error(), Retained: false,
			}
			record.EndedAt = time.Now().UTC()
			record.ExitCode = exitCode
			record.Error = err.Error()
			record.Outcome = "not_started"
			record.Outbound = *outboundEnabled
		}); updateErr != nil {
			fmt.Fprintf(stderr, "unring: record preflight refusal: %v\n", updateErr)
		}
		fmt.Fprintf(stderr, "unring: choose file snapshot scope: %v\n", err)
		return exitCode
	}
	watchPaths := scope.Watched
	if capBytes < 0 {
		capBytes, err = localrollback.RetentionCapForState(auditStore.StateDir())
		if err != nil {
			fmt.Fprintf(stderr, "unring: read snapshot retention cap: %v\n", err)
			return usageExitCode
		}
	}
	if err := localrollback.SaveRetentionCap(auditStore.StateDir(), capBytes); err != nil {
		fmt.Fprintf(stderr, "unring: save snapshot retention cap: %v\n", err)
		return internalErrorExitCode
	}
	if scope.ScanRoot != "" && os.Getenv("UNRING_TEST_DISABLE_VOLUME_BACKSTOP") == "" {
		fmt.Fprintf(stderr,
			"unring: scanning %s for the widened change list; this metadata scan may take several seconds and excludes %s.\n",
			scope.ScanRoot, homeScanExclusions)
	} else if scope.ScanError != "" {
		fmt.Fprintf(stderr, "unring: WIDENED CHANGE LIST UNAVAILABLE: %s\n", scope.ScanError)
	}
	fileSession, fileSummary, err := localrollback.StartScope(
		auditStore.StateDir(), auditRecord.ID, scope, capBytes, auditRecord.StartedAt,
	)
	if err != nil {
		_ = auditSession.Update(func(record *audit.Record) {
			record.Files = localrollback.Summary{
				Watched: watchPaths, Complete: false, Error: err.Error(),
				RetentionCap: capBytes, Retained: false,
			}
			record.EndedAt = time.Now().UTC()
			record.ExitCode = internalErrorExitCode
			record.Error = err.Error()
			record.Outcome = "not_started"
		})
		fmt.Fprintf(stderr, "unring: create file snapshot: %v\n", err)
		return internalErrorExitCode
	}
	if err := auditSession.Update(func(record *audit.Record) {
		record.Files = fileSummary
		record.Outbound = *outboundEnabled
		if !*outboundEnabled {
			record.HTTPS = httpsproxy.Summary{Sealed: true, Finalized: true}
			record.GH = ghshim.Summary{Sealed: true}
		}
	}); err != nil {
		fmt.Fprintf(stderr, "unring: record file snapshot: %v\n", err)
		return internalErrorExitCode
	}
	printSnapshotStarted(stderr, fileSummary)
	for _, evictedID := range fileSummary.Evicted {
		if evictedRecord, loadErr := auditStore.Load(evictedID); loadErr == nil {
			evictedRecord.Files.Retained = false
			_ = auditStore.Save(evictedRecord)
		}
	}

	var proxy postgresSession
	var httpsProxy *httpsproxy.Proxy
	var ghSession *ghshim.Session
	var finalized bool
	var auditError string
	requestedDecision := "discard"
	defer func() {
		recovered := recover()
		outcome := ""
		if proxy != nil && !finalized {
			closeErr := proxy.Close()
			if closeErr == nil {
				outcome = "discarded"
			} else {
				outcome = "unknown"
				auditError = joinErrorText(auditError, closeErr)
			}
		}
		if httpsProxy != nil {
			if closeErr := httpsProxy.Close(); closeErr != nil {
				auditError = joinErrorText(auditError, closeErr)
			}
		}
		if ghSession != nil {
			if closeErr := ghSession.Close(); closeErr != nil {
				auditError = joinErrorText(auditError, closeErr)
			}
		}
		if recovered != nil {
			auditError = fmt.Sprintf("panic: %v", recovered)
			if exitCode == 0 {
				exitCode = internalErrorExitCode
			}
		}
		saveErr := auditSession.Update(func(record *audit.Record) {
			record.EndedAt = time.Now().UTC()
			record.ExitCode = exitCode
			record.Error = strings.TrimPrefix(auditError, "\n")
			record.Decision = requestedDecision
			record.Outbound = *outboundEnabled
			record.Files = fileSummary
			if proxy != nil {
				record.Postgres = proxy.Summary()
			}
			if httpsProxy != nil {
				updateHTTPSAudit(record, httpsProxy.Summary())
			}
			if ghSession != nil {
				record.GH = ghSession.Summary()
			}
			if outcome != "" {
				record.Outcome = outcome
			} else if record.Outcome == "pending" {
				record.Outcome = "not_started"
			}
		})
		if saveErr != nil {
			fmt.Fprintf(stderr, "unring: write audit log: %v\n", saveErr)
			if exitCode == 0 {
				exitCode = internalErrorExitCode
			}
		}
		if recovered != nil {
			panic(recovered)
		}
	}()

	backendConfig, databaseConfigured, err := parseOptionalBackendConfig()
	if err != nil {
		auditError = err.Error()
		fmt.Fprintf(stderr, "unring: %v\n", err)
		return internalErrorExitCode
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	approvalRequests := make(chan runner.ApprovalRequest)
	var adapterSet *adapter.Set
	if *outboundEnabled {
		adapterSet, err = loadAdapters(os.Getenv("UNRING_ADAPTERS"))
		if err != nil {
			cancel()
			auditError = err.Error()
			fmt.Fprintf(stderr, "unring: load HTTPS adapters: %v\n", err)
			return internalErrorExitCode
		}
		ghSession, err = ghshim.Start(ghshim.Options{
			Adapters: adapterSet, Stdin: stdin, Stdout: stdout, Stderr: stderr,
			Approve: func(approvalContext context.Context, request ghshim.ApprovalRequest) (bool, error) {
				reply := make(chan runner.ApprovalResult, 1)
				work := runner.ApprovalRequest{
					Decide: func() (bool, error) {
						return promptGHApproval(stdin, stdout, request), nil
					},
					Reply: reply,
				}
				select {
				case approvalRequests <- work:
				case <-approvalContext.Done():
					return false, approvalContext.Err()
				}
				select {
				case result := <-reply:
					decision := "declined"
					if result.Approved {
						decision = "approved"
					}
					approvalError := ""
					if result.Err != nil {
						decision = "error"
						approvalError = result.Err.Error()
					}
					if err := auditSession.Update(func(record *audit.Record) {
						record.Approvals = append(record.Approvals, audit.Approval{
							Kind: "gh", Statement: request.Invocation, Reason: request.Reason,
							Decision: decision, Error: approvalError, Time: time.Now().UTC(),
						})
					}); err != nil {
						return false, fmt.Errorf("record gh approval decision: %w", err)
					}
					return result.Approved, result.Err
				case <-approvalContext.Done():
					return false, approvalContext.Err()
				}
			},
		})
		if err != nil {
			cancel()
			auditError = err.Error()
			fmt.Fprintf(stderr, "unring: start per-session gh shim: %v\n", err)
			return internalErrorExitCode
		}
	}
	if databaseConfigured {
		startedProxy, startErr := pgproxy.StartWithOptions(ctx, backendConfig, pgproxy.Options{
			Approve: func(approvalContext context.Context, request pgproxy.ApprovalRequest) (bool, error) {
				reply := make(chan runner.ApprovalResult, 1)
				work := runner.ApprovalRequest{
					Decide: func() (bool, error) {
						return promptIrreversibleApproval(stdin, stdout, request), nil
					},
					Reply: reply,
				}
				select {
				case approvalRequests <- work:
				case <-approvalContext.Done():
					return false, approvalContext.Err()
				}
				select {
				case result := <-reply:
					decision := "declined"
					if result.Approved {
						decision = "approved"
					}
					approvalError := ""
					if result.Err != nil {
						decision = "error"
						approvalError = result.Err.Error()
					}
					if err := auditSession.Update(func(record *audit.Record) {
						record.Approvals = append(record.Approvals, audit.Approval{
							Kind: "postgres", Statement: request.SQL, Reason: request.Reason,
							Decision: decision, Error: approvalError, Time: time.Now().UTC(),
						})
					}); err != nil {
						return false, fmt.Errorf("record irreversible-action decision: %w", err)
					}
					return result.Approved, result.Err
				case <-approvalContext.Done():
					return false, approvalContext.Err()
				}
			},
		})
		err = startErr
		if startErr == nil {
			proxy = startedProxy
		}
	}
	cancel()
	if err != nil {
		auditError = err.Error()
		fmt.Fprintf(stderr, "unring: start postgres session: %v\n", err)
		fmt.Fprintln(stderr,
			"unring: check DATABASE_URL, database reachability and credentials, and that the server is PostgreSQL 14 or newer")
		return internalErrorExitCode
	}

	var authority *httpsproxy.Authority
	if *outboundEnabled {
		authority, err = httpsproxy.EnsureAuthority(auditStore.StateDir())
		if err != nil {
			auditError = err.Error()
			fmt.Fprintf(stderr, "unring: initialize per-user HTTPS CA: %v\n", err)
			return internalErrorExitCode
		}
		httpsProxy, err = httpsproxy.Start(authority, httpsproxy.Options{
			PassthroughHost:   configuredPassthroughHosts(os.Getenv("UNRING_HTTPS_PASSTHROUGH")),
			AgentControlPlane: configuredAgentControlPlane(command),
			Adapters:          adapterSet,
			StagedUpdated:     stagedAuditUpdater(auditSession),
			Approve: func(approvalContext context.Context, request httpsproxy.ApprovalRequest) (bool, error) {
				reply := make(chan runner.ApprovalResult, 1)
				work := runner.ApprovalRequest{
					Decide: func() (bool, error) {
						return promptHTTPSApproval(stdin, stdout, request), nil
					},
					Reply: reply,
				}
				select {
				case approvalRequests <- work:
				case <-approvalContext.Done():
					return false, approvalContext.Err()
				}
				select {
				case result := <-reply:
					decision := "declined"
					if result.Approved {
						decision = "approved"
					}
					approvalError := ""
					if result.Err != nil {
						decision = "error"
						approvalError = result.Err.Error()
					}
					if err := auditSession.Update(func(record *audit.Record) {
						record.Approvals = append(record.Approvals, audit.Approval{
							Kind: "https", Statement: request.Method + " " + request.URL,
							Reason: request.Reason, Decision: decision,
							Error: approvalError, Time: time.Now().UTC(),
						})
					}); err != nil {
						return false, fmt.Errorf("record HTTPS approval decision: %w", err)
					}
					return result.Approved, result.Err
				case <-approvalContext.Done():
					return false, approvalContext.Err()
				}
			},
		})
		if err != nil {
			auditError = err.Error()
			fmt.Fprintf(stderr, "unring: start HTTPS proxy: %v\n", err)
			return internalErrorExitCode
		}
	}

	childEnvironment := os.Environ()
	if databaseConfigured {
		childEnvironment, err = childenv.Postgres(
			childEnvironment, proxy.Address(), backendConfig,
		)
		if err != nil {
			auditError = err.Error()
			fmt.Fprintf(stderr, "unring: build child environment: %v\n", err)
			return internalErrorExitCode
		}
	}
	if *outboundEnabled {
		childEnvironment, err = childenv.HTTPS(
			childEnvironment, httpsProxy.Address(), authority.CertificatePath,
		)
		if err != nil {
			auditError = err.Error()
			fmt.Fprintf(stderr, "unring: build child HTTPS environment: %v\n", err)
			return internalErrorExitCode
		}
		childEnvironment = ghSession.Environment(childEnvironment)
	}

	signalChannel := make(chan os.Signal, 2)
	signal.Notify(signalChannel, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(signalChannel)

	if !databaseConfigured {
		// Assign the no-op database session only when every startup step has
		// succeeded and the child is about to run. Earlier failures must retain
		// the audit record's not_started outcome.
		proxy = newUnconfiguredPostgresSession()
	}
	result := runner.Run(runner.Options{
		Command:   command,
		Env:       childEnvironment,
		Stdin:     stdin,
		Stdout:    stdout,
		Stderr:    stderr,
		Signals:   signalChannel,
		Abort:     proxy.Done(),
		Approvals: approvalRequests,
	})
	interrupted := result.Interrupted || pendingSignal(signalChannel)
	if result.Err != nil {
		auditError = joinErrorText(auditError, result.Err)
	}
	printScanFinishing(stderr, fileSummary)
	fileSummary = fileSession.Seal(time.Now())
	printScanFinished(stderr, fileSummary)
	if evicted, usage, retentionErr := localrollback.EnforceRetention(
		auditStore.StateDir(), capBytes, auditRecord.ID,
	); retentionErr != nil {
		fmt.Fprintf(stderr, "unring: enforce snapshot retention: %v\n", retentionErr)
	} else {
		for _, evictedID := range evicted {
			fileSummary.Evicted = append(fileSummary.Evicted, evictedID)
			if evictedID == auditRecord.ID {
				fileSummary.Retained = false
			}
			fmt.Fprintf(stderr, "unring: retention evicted oldest snapshot %s.\n", evictedID)
			if evictedRecord, loadErr := auditStore.Load(evictedID); loadErr == nil {
				evictedRecord.Files.Retained = false
				_ = auditStore.Save(evictedRecord)
			}
		}
		if usage.Exact && usage.Bytes > usage.CapBytes {
			fmt.Fprintf(stderr,
				"unring: current snapshot remains retained; measured snapshot usage is %d bytes, above the %d-byte cap. It becomes eligible for eviction after this session.\n",
				usage.Bytes, usage.CapBytes)
		}
	}
	if !fileSummary.Complete {
		if hasActionableFileCoverageFailure(fileSummary) {
			fmt.Fprintf(stderr, "unring: FILE COVERAGE INCOMPLETE: %s\n", fileSummary.Error)
		} else {
			fmt.Fprintln(stderr, "unring: FILE COVERAGE NOTE: unsupported file types remain recorded but cannot be restored per path.")
		}
	}
	printFileChanges(stdout, auditRecord.ID, fileSummary)
	if err := auditSession.Update(func(record *audit.Record) { record.Files = fileSummary }); err != nil {
		auditError = joinErrorText(auditError, err)
		fmt.Fprintf(stderr, "unring: record file changes: %v\n", err)
	}

	ghSummary := ghshim.Summary{Sealed: true}
	httpsSummary := httpsproxy.Summary{Sealed: true, Finalized: true}
	var ghSealErr error
	var httpsSealErr error
	if *outboundEnabled {
		ghSealContext, ghSealCancel := context.WithTimeout(context.Background(), 10*time.Second)
		ghSealErr = ghSession.Seal(ghSealContext)
		ghSealCancel()
		ghSummary = ghSession.Summary()

		httpsSealContext, httpsSealCancel := context.WithTimeout(context.Background(), 10*time.Second)
		httpsSealErr = httpsProxy.Seal(httpsSealContext)
		httpsSealCancel()
		httpsSummary = httpsProxy.Summary()
	}

	sealContext, sealCancel := context.WithTimeout(context.Background(), 10*time.Second)
	sealErr := proxy.Seal(sealContext)
	sealCancel()
	summary := proxy.Summary()

	postgresInterceptionErr := sealErr
	if fatalErr := proxy.Err(); postgresInterceptionErr == nil && fatalErr != nil {
		postgresInterceptionErr = fatalErr
	}
	if postgresInterceptionErr != nil {
		auditError = joinErrorText(auditError, postgresInterceptionErr)
		fmt.Fprintf(
			stderr,
			"unring: INTERCEPTION LOST: the real Postgres outcome is unknown; "+
				"unring will not claim this session was safely discarded: %v\n",
			postgresInterceptionErr,
		)
	}
	if httpsSealErr != nil {
		auditError = joinErrorText(auditError, httpsSealErr)
		fmt.Fprintf(stderr,
			"unring: HTTPS INTERCEPTION LOST: the HTTPS audit may be incomplete; "+
				"the database session will be discarded: %v\n", httpsSealErr)
	}
	if ghSealErr != nil {
		auditError = joinErrorText(auditError, ghSealErr)
		fmt.Fprintf(stderr,
			"unring: GH SHIM LOST: gh activity may be incomplete; the session will be discarded: %v\n",
			ghSealErr)
	}
	interceptionErr := errors.Join(postgresInterceptionErr, httpsSealErr, ghSealErr)

	if interceptionErr == nil && !summary.HasReviewableActivity() &&
		!httpsSummary.HasReviewableActivity() && !ghSummary.HasReviewableActivity() {
		if result.Err != nil {
			fmt.Fprintf(stderr, "unring: %v\n", result.Err)
		}
		if hasOnlyObservedHTTPSActivity(summary, httpsSummary, ghSummary) {
			// Safe reads and enumerated agent control-plane calls do not need a
			// commit decision, but they were intercepted and must remain visible.
			printObservedSummaryWithExternal(stdout, summary, httpsSummary, ghSummary)
		} else if summary.InterceptionStatus == pgproxy.InterceptionNotConfigured {
			if !*outboundEnabled {
				printOutboundDisabled(stdout)
			}
			printCoverageOnlyReview(stdout, summary)
		} else {
			fmt.Fprintln(stderr, quietSessionDisclosure)
		}
		finalizeContext, finalizeCancel := context.WithTimeout(context.Background(), 10*time.Second)
		var ghFinalizeErr error
		var httpsFinalizeErr error
		if *outboundEnabled {
			ghFinalizeErr = ghSession.Finalize(finalizeContext, false)
			httpsFinalizeErr = httpsProxy.Finalize(finalizeContext, false)
		}
		finalizeErr := errors.Join(
			ghFinalizeErr, httpsFinalizeErr,
			proxy.Finalize(finalizeContext, pgproxy.DecisionRollback),
		)
		finalizeCancel()
		if finalizeErr != nil {
			finalized = true
			auditError = finalizeErr.Error()
			_ = auditSession.Update(func(record *audit.Record) {
				record.Postgres = summary
				if *outboundEnabled {
					updateHTTPSAudit(record, httpsProxy.Summary())
				}
				record.GH = ghSummary
				record.Files = fileSummary
				record.Decision = "discard"
				record.Outcome = "unknown"
			})
			fmt.Fprintf(stderr, "unring: session outcome not confirmed: %v\n", finalizeErr)
			return internalErrorExitCode
		}
		finalized = true
		if err := auditSession.Update(func(record *audit.Record) {
			record.Postgres = summary
			if *outboundEnabled {
				updateHTTPSAudit(record, httpsProxy.Summary())
			}
			record.GH = ghSummary
			record.Files = fileSummary
			record.Decision = "discard"
			record.Outcome = "discarded"
		}); err != nil {
			auditError = err.Error()
			fmt.Fprintf(stderr, "unring: write audit log: %v\n", err)
			return internalErrorExitCode
		}
		return result.ExitCode
	}

	useTUI := interceptionErr == nil && !interrupted && result.Err == nil &&
		summary.Changes.Complete && !*forceCommit && !*forceDiscard && shouldUseTUI(stdin, stdout)
	if !useTUI {
		if !*outboundEnabled {
			printOutboundDisabled(stdout)
		}
		printSummaryWithExternal(stdout, summary, httpsSummary, ghSummary)
	}

	decision := pgproxy.DecisionRollback
	switch {
	case interceptionErr != nil:
		// Rollback remains the only safe request, but Finalize will report
		// that it cannot confirm the outcome.
	case !summary.Changes.Complete:
		fmt.Fprintln(stderr,
			"unring: the sealed change summary is incomplete; discarding instead of offering commit")
	case interrupted:
		fmt.Fprintln(stdout, "Signal received: discarding the session.")
	case result.Err != nil:
		fmt.Fprintf(stderr, "unring: %v\n", result.Err)
	case *forceCommit:
		decision = pgproxy.DecisionCommit
	case *forceDiscard:
		// Rollback is already the safe default.
	default:
		if useTUI {
			var reviewErr error
			decision, interrupted, reviewErr = reviewDecisionWithSignal(
				stdin, stdout, signalChannel, summary, httpsSummary, ghSummary,
			)
			if reviewErr != nil {
				fmt.Fprintf(stderr, "unring: %v; defaulting to discard\n", reviewErr)
				decision = pgproxy.DecisionRollback
			}
		} else {
			var promptInterrupted bool
			decision, promptInterrupted = promptDecisionWithSignal(stdin, stdout, signalChannel)
			interrupted = interrupted || promptInterrupted
		}
	}

	if pendingSignal(signalChannel) {
		interrupted = true
	}
	if interrupted {
		decision = pgproxy.DecisionRollback
	}

	if err := auditSession.Update(func(record *audit.Record) {
		record.Postgres = summary
		updateHTTPSAudit(record, httpsSummary)
		record.GH = ghSummary
		record.Decision = auditDecision(decision)
	}); err != nil {
		auditError = err.Error()
		fmt.Fprintf(stderr, "unring: write audit log before final decision: %v\n", err)
		decision = pgproxy.DecisionRollback
	}
	requestedDecision = auditDecision(decision)
	commitExternal := decision == pgproxy.DecisionCommit
	var ghFinalizeErr error
	if *outboundEnabled {
		ghFinalizeContext, ghFinalizeCancel := context.WithTimeout(context.Background(), 30*time.Second)
		ghFinalizeErr = ghSession.Finalize(ghFinalizeContext, commitExternal)
		ghFinalizeCancel()
	}
	if ghFinalizeErr != nil {
		auditError = joinErrorText(auditError, ghFinalizeErr)
	}
	commitHTTPS := commitExternal && ghFinalizeErr == nil
	var httpsFinalizeErr error
	if *outboundEnabled {
		httpsFinalizeContext, httpsFinalizeCancel := context.WithTimeout(context.Background(), 30*time.Second)
		httpsFinalizeErr = httpsProxy.Finalize(httpsFinalizeContext, commitHTTPS)
		httpsFinalizeCancel()
	}
	postgresDecision := decision
	if httpsFinalizeErr != nil || ghFinalizeErr != nil {
		// A staged HTTP replay may have partially succeeded. Keep the database
		// reversible instead of compounding that uncertainty with a commit.
		postgresDecision = pgproxy.DecisionRollback
		auditError = joinErrorText(auditError, httpsFinalizeErr)
	}
	finalizeContext, finalizeCancel := context.WithTimeout(context.Background(), 10*time.Second)
	postgresFinalizeErr := proxy.Finalize(finalizeContext, postgresDecision)
	finalizeCancel()
	finalizeErr := errors.Join(ghFinalizeErr, httpsFinalizeErr, postgresFinalizeErr)
	finalized = true
	if finalizeErr != nil {
		auditError = joinErrorText(auditError, postgresFinalizeErr)
		finalHTTPS := httpsSummary
		if *outboundEnabled {
			finalHTTPS = httpsProxy.Summary()
		}
		_ = auditSession.Update(func(record *audit.Record) {
			record.Postgres = proxy.Summary()
			if *outboundEnabled {
				updateHTTPSAudit(record, finalHTTPS)
				record.GH = ghSession.Summary()
			}
			record.Files = fileSummary
			record.Outcome = "unknown"
		})
		if commitHTTPS && httpsFinalizeErr != nil {
			printPartialCommitOutcome(stdout, finalHTTPS, postgresFinalizeErr)
		}
		if *outboundEnabled && ghFinalizeErr != nil {
			fmt.Fprintln(stdout, "\nGH DISCARD COMPENSATION FAILED OR WAS IMPOSSIBLE")
			fmt.Fprintln(stdout,
				"An approved gh mutation really ran and may still exist. Unring is not claiming it was undone.")
			printGHSummary(stdout, ghSession.Summary())
		}
		printCompensationFailures(stdout, finalHTTPS)
		fmt.Fprintf(stderr, "unring: session outcome not confirmed: %v\n", finalizeErr)
		return internalErrorExitCode
	}
	if err := auditSession.Update(func(record *audit.Record) {
		record.Postgres = proxy.Summary()
		if *outboundEnabled {
			updateHTTPSAudit(record, httpsProxy.Summary())
			record.GH = ghSession.Summary()
		}
		record.Files = fileSummary
		record.Outcome = pastTense(decision)
	}); err != nil {
		auditError = err.Error()
		fmt.Fprintf(stderr, "unring: write final audit log: %v\n", err)
		return internalErrorExitCode
	}
	fmt.Fprintf(stdout, "Session %s.\n", pastTense(decision))

	if result.Err != nil {
		return result.ExitCode
	}
	return result.ExitCode
}

func printSnapshotStarted(output io.Writer, summary localrollback.Summary) {
	if summary.ScanRoot != "" && os.Getenv("UNRING_TEST_DISABLE_VOLUME_BACKSTOP") == "" {
		fmt.Fprintf(output,
			"unring: widened change-list baseline scanned %d entries in %d ms; the same pruned scope will be scanned again after the child exits.\n",
			summary.ScanBeforeFiles, summary.ScanBeforeMillis)
	}
	if summary.Backstop.Checked && !summary.Backstop.Available {
		fmt.Fprintln(output, "unring: ============================================================")
		fmt.Fprintln(output, "unring: NO WHOLE-VOLUME BACKSTOP")
		fmt.Fprintf(output, "unring: %s.\n", strings.TrimSuffix(summary.Backstop.Reason, "."))
		fmt.Fprintln(output, "unring: Changes outside the clone scope can be reported, but their prior contents cannot be restored.")
		fmt.Fprintln(output, "unring: The child will still run with clone-based protection for watched paths.")
		fmt.Fprintln(output, "unring: ============================================================")
	} else if summary.Backstop.Available {
		for _, snapshot := range summary.Backstop.Snapshots {
			fmt.Fprintf(output, "unring: whole-volume backstop recorded %s on %s.\n", snapshot.Name, snapshot.MountPoint)
		}
	}
	printChangeListLimitation(output, summary, "unring: ")
	for _, failure := range summary.Backstop.Excluded {
		fmt.Fprintf(output, "unring: PATH OUTSIDE WHOLE-VOLUME BACKSTOP: %s: %s\n", failure.Path, failure.Error)
	}
	if strings.Contains(summary.Storage, "full-copy") {
		fmt.Fprintf(output,
			"unring: snapshot cloning was unavailable for %d bytes; those bytes were copied in full before the child started.\n",
			summary.CopiedBytes)
	}
	for _, failure := range summary.Uncaptured {
		printCaptureFailure(output, "unring: ", failure)
	}
	for _, failure := range summary.ScanFailures {
		fmt.Fprintf(output, "unring: CHANGE-LIST SCAN INCOMPLETE: %s: %s\n", failure.Path, failure.Error)
	}
	for _, evicted := range summary.Evicted {
		fmt.Fprintf(output, "unring: retention evicted oldest snapshot %s.\n", evicted)
	}
	if summary.StorageExact && summary.StorageBytes > summary.RetentionCap {
		fmt.Fprintf(output,
			"unring: current snapshot uses %d measured bytes, above the %d-byte cap; it is retained for this session and becomes eligible for eviction later.\n",
			summary.StorageBytes, summary.RetentionCap)
	} else if !summary.StorageExact {
		fmt.Fprintf(output,
			"unring: snapshot storage is an upper-bound estimate of %d bytes; retention will not evict snapshots based on that estimate.\n",
			summary.StorageBytes)
	}
}

func printChangeListLimitation(output io.Writer, summary localrollback.Summary, prefix string) {
	if os.Getenv("UNRING_TEST_DISABLE_VOLUME_BACKSTOP") != "" || summary.ChangeListScope == "" {
		return
	}
	fmt.Fprintf(output, "%s============================================================\n", prefix)
	fmt.Fprintf(output, "%sCHANGE LIST IS NOT WHOLE-VOLUME\n", prefix)
	switch summary.ChangeListScope {
	case localrollback.ChangeListScopeWatchOnly:
		fmt.Fprintf(output, "%sIt is limited by --watch-only to: %s\n", prefix, strings.Join(summary.ChangeListRoots, ", "))
		fmt.Fprintf(output, "%sChanges elsewhere are not reported or written to the audit record, even when the APFS snapshot contains them; the session can look clean after such a change.\n", prefix)
	case localrollback.ChangeListScopeHomeAndClone:
		fmt.Fprintf(output, "%sChange reporting covers the home scan and clone roots: %s\n", prefix, strings.Join(summary.ChangeListRoots, ", "))
		fmt.Fprintf(output, "%sChanges outside the home scan and clone roots are not reported, including /etc, /opt, /tmp, or another volume, even when the APFS snapshot contains them.\n", prefix)
	default:
		fmt.Fprintf(output, "%sChange reporting is limited to the clone roots: %s\n", prefix, strings.Join(summary.ChangeListRoots, ", "))
		fmt.Fprintf(output, "%sChanges elsewhere are not reported, even when a volume snapshot contains them.\n", prefix)
	}
	fmt.Fprintf(output, "%s============================================================\n", prefix)
}

func printStoredChangeListLimitation(output io.Writer, summary localrollback.Summary, prefix string, includeScanFailures bool) {
	printChangeListLimitation(output, summary, prefix)
	if os.Getenv("UNRING_TEST_DISABLE_VOLUME_BACKSTOP") != "" {
		return
	}
	if summary.ChangeListScope == localrollback.ChangeListScopeHomeAndClone {
		fmt.Fprintf(output, "%sHome scan exclusions: %s.\n", prefix, homeScanExclusions)
		if len(summary.ScanExcluded) > 0 {
			fmt.Fprintf(output, "%sHome scan excluded paths recorded for this session: %s.\n",
				prefix, strings.Join(summary.ScanExcluded, ", "))
		}
	}
	if includeScanFailures && len(summary.ScanFailures) > 0 {
		paths := make([]string, 0, len(summary.ScanFailures))
		for _, failure := range summary.ScanFailures {
			path := failure.Path
			if path == "" {
				path = "unknown path"
			}
			paths = append(paths, path)
		}
		fmt.Fprintf(output, "%sCHANGE-LIST SCAN INCOMPLETE at recorded paths: %s.\n",
			prefix, strings.Join(paths, ", "))
	}
}

func printScanFinishing(output io.Writer, summary localrollback.Summary) {
	if summary.ScanRoot == "" || os.Getenv("UNRING_TEST_DISABLE_VOLUME_BACKSTOP") != "" {
		return
	}
	fmt.Fprintf(output,
		"unring: rescanning %s and checking Time Machine inclusion for changed paths; large change sets may take time.\n",
		summary.ScanRoot)
}

func printScanFinished(output io.Writer, summary localrollback.Summary) {
	if summary.ScanRoot == "" || os.Getenv("UNRING_TEST_DISABLE_VOLUME_BACKSTOP") != "" {
		return
	}
	fmt.Fprintf(output, "unring: widened change-list final scan inspected %d entries in %d ms.\n",
		summary.ScanAfterFiles, summary.ScanAfterMillis)
}

func printFileChanges(output io.Writer, sessionID string, summary localrollback.Summary) {
	if len(summary.Changes) == 0 {
		return
	}
	created, modified, deleted := 0, 0, 0
	for _, change := range summary.Changes {
		switch change.Kind {
		case "created":
			created++
		case "modified":
			modified++
		case "deleted":
			deleted++
		}
	}
	fmt.Fprintf(output,
		"Files changed: %d created, %d modified, %d deleted. No file decision is needed now.\n",
		created, modified, deleted)
	regular, agentState := groupAgentStateChanges(summary.Changes)
	printLiveChanges(output, regular)
	if len(agentState) > 0 {
		fmt.Fprintln(output, "AGENT OWN-STATE CHANGES — reported, but skipped by restore --all unless explicitly included")
		printLiveChanges(output, agentState)
	}
	if summary.Retained && hasRestorableFileChange(summary.Changes) {
		fmt.Fprintf(output, "Restore later with: unring restore %s\n", sessionID)
	} else if summary.Retained {
		fmt.Fprintln(output, "No changed path has restorable snapshot data.")
	} else {
		fmt.Fprintln(output, "Snapshot data is no longer retained; these paths cannot be restored from this session.")
	}
}

func printLiveChanges(output io.Writer, changes []localrollback.Change) {
	for _, change := range changes {
		fmt.Fprintf(output, "  %-8s %s\n", change.Kind, change.Path)
		if change.RestoreSource == localrollback.RestoreSourceVolume {
			fmt.Fprintln(output, "             SNAPSHOT ONLY: restoring this path requires sudo to mount the APFS snapshot.")
		} else if change.UnrestorableReason != "" {
			fmt.Fprintf(output, "             NOT RESTORABLE: %s\n", change.UnrestorableReason)
		}
	}
}

func printAuditFiles(output io.Writer, summary localrollback.Summary) {
	if len(summary.Watched) == 0 {
		return
	}
	fmt.Fprintln(output, "\nFILES — RESTORE COVERAGE")
	storageLabel := "measured storage bytes"
	if !summary.StorageExact {
		storageLabel = "storage-byte upper bound"
	}
	fmt.Fprintf(output, "  Snapshot: %s; %d %s (%d logical); retained: %t\n",
		summary.Storage, summary.StorageBytes, storageLabel, summary.LogicalBytes, summary.Retained)
	for _, failure := range summary.Uncaptured {
		printCaptureFailure(output, "  ", failure)
	}
	if summary.Backstop.Available {
		for _, snapshot := range summary.Backstop.Snapshots {
			fmt.Fprintf(output, "  Volume backstop: %s on %s\n", snapshot.Name, snapshot.MountPoint)
		}
	} else {
		fmt.Fprintf(output, "  NO WHOLE-VOLUME BACKSTOP: %s\n", summary.Backstop.Reason)
	}
	for _, failure := range summary.Backstop.Excluded {
		fmt.Fprintf(output, "  OUTSIDE VOLUME BACKSTOP: %s: %s\n", failure.Path, failure.Error)
	}
	printStoredChangeListLimitation(output, summary, "  ", false)
	for _, failure := range summary.ScanFailures {
		fmt.Fprintf(output, "  CHANGE-LIST SCAN INCOMPLETE: %s: %s\n", failure.Path, failure.Error)
	}
	if summary.ScanRoot != "" {
		fmt.Fprintf(output, "  Change-list scan: %d entries/%d ms before; %d entries/%d ms after\n",
			summary.ScanBeforeFiles, summary.ScanBeforeMillis,
			summary.ScanAfterFiles, summary.ScanAfterMillis)
	}
	regular, agentState := groupAgentStateChanges(summary.Changes)
	printStoredChanges(output, regular)
	if len(agentState) > 0 {
		fmt.Fprintln(output, "  AGENT OWN-STATE CHANGES — reported, but skipped by restore --all unless explicitly included")
		printStoredChanges(output, agentState)
	}
	if summary.Error != "" {
		if hasActionableFileCoverageFailure(summary) {
			fmt.Fprintf(output, "  INCOMPLETE: %s\n", summary.Error)
		} else {
			fmt.Fprintf(output, "  Coverage note: %s\n", summary.Error)
		}
	}
}

func printCaptureFailure(output io.Writer, prefix string, failure localrollback.CaptureFailure) {
	if localrollback.IsUnsupportedFileTypeFailure(failure) {
		fmt.Fprintf(output, "%sUNSUPPORTED FILE TYPE (informational): %s: %s\n", prefix, failure.Path, failure.Error)
		return
	}
	fmt.Fprintf(output, "%sFILE NOT SNAPSHOTTED: %s: %s\n", prefix, failure.Path, failure.Error)
}

func hasActionableFileCoverageFailure(summary localrollback.Summary) bool {
	for _, failure := range summary.Uncaptured {
		if !localrollback.IsUnsupportedFileTypeFailure(failure) {
			return true
		}
	}
	return len(summary.ScanFailures) > 0
}

func printStoredChanges(output io.Writer, changes []localrollback.Change) {
	for _, change := range changes {
		fmt.Fprintf(output, "  %-8s %s\n", change.Kind, change.Path)
		if change.RestoreSource == localrollback.RestoreSourceVolume {
			fmt.Fprintln(output, "             SNAPSHOT ONLY: requires sudo to mount the APFS snapshot")
		} else if change.UnrestorableReason != "" {
			fmt.Fprintf(output, "             NOT RESTORABLE: %s\n", change.UnrestorableReason)
		}
	}
}

func groupAgentStateChanges(changes []localrollback.Change) ([]localrollback.Change, []localrollback.Change) {
	regular := make([]localrollback.Change, 0, len(changes))
	agentState := make([]localrollback.Change, 0)
	for _, change := range changes {
		if isAgentStateChange(change) {
			agentState = append(agentState, change)
		} else {
			regular = append(regular, change)
		}
	}
	return regular, agentState
}

func isAgentStateChange(change localrollback.Change) bool {
	return localrollback.IsAgentStatePath(change.Path, "")
}

func printOutboundDisabled(output io.Writer) {
	fmt.Fprintln(output, "\nOUTBOUND INTERCEPTION DISABLED — HTTPS, adapters, and the gh shim were not started.")
	fmt.Fprintln(output, "Use --outbound on a future session to opt in; restoring files cannot recall data already sent.")
}

func printCompensationFailures(output io.Writer, summary httpsproxy.Summary) {
	printedHeader := false
	printOne := func(method, target string, undo *httpsproxy.UndoRecord) {
		if undo == nil || (undo.State != "failed" && undo.State != "unavailable") {
			return
		}
		if !printedHeader {
			fmt.Fprintln(output, "\n!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!")
			fmt.Fprintln(output, "DISCARD COMPENSATION FAILED OR WAS IMPOSSIBLE")
			fmt.Fprintln(output,
				"The original external effect may still exist. Unring is not claiming it was undone.")
			fmt.Fprintln(output, "!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!")
			printedHeader = true
		}
		fmt.Fprintf(output, "  - %s %s\n", method, target)
		fmt.Fprintf(output, "    Attempted: %s\n", undo.Effect)
		if undo.Error != "" {
			fmt.Fprintf(output, "    Error: %s\n", undo.Error)
		}
		fmt.Fprintf(output, "    WHAT REMAINS: %s\n", undo.StillExists)
	}
	for _, request := range summary.Requests {
		printOne(request.Method, request.URL, request.Undo)
	}
	for _, request := range summary.Staged {
		printOne(request.Method, request.URL, request.Undo)
	}
}

func printPartialCommitOutcome(
	output io.Writer,
	summary httpsproxy.Summary,
	postgresFinalizeErr error,
) {
	fmt.Fprintln(output, "\nUNRING COMMIT DID NOT COMPLETE")
	fmt.Fprintln(output,
		"Some staged HTTPS delivery outcomes are irreversible or unknown; inspect every item below.")
	for _, request := range summary.Staged {
		fmt.Fprintf(output, "  - [%s] %s %s\n", request.State, request.Method, request.URL)
		if request.ReplayStatusCode != 0 {
			fmt.Fprintf(output, "    Origin status: HTTP %d\n", request.ReplayStatusCode)
		}
		if request.Warning != "" {
			fmt.Fprintf(output, "    Warning: %s\n", request.Warning)
		}
		if request.Error != "" {
			fmt.Fprintf(output, "    Error: %s\n", request.Error)
		}
	}
	if summary.HasForwardedEffects() {
		fmt.Fprintln(output,
			"Already-forwarded HTTPS requests remain as sent. Commit never runs discard compensation:")
		for _, request := range summary.Requests {
			if request.Disposition == httpsproxy.RequestDispositionSafeRead ||
				request.Disposition == httpsproxy.RequestDispositionControlPlane {
				continue
			}
			fmt.Fprintf(output, "  - %s %s\n", request.Method, request.URL)
		}
	}
	if postgresFinalizeErr == nil {
		fmt.Fprintln(output,
			"Postgres transaction: DISCARDED. The requested commit became a rollback because HTTPS replay was not fully confirmed.")
	} else {
		fmt.Fprintf(output,
			"Postgres transaction: UNKNOWN. Rollback could not be confirmed: %v\n",
			postgresFinalizeErr,
		)
	}
}

func joinErrorText(existing string, err error) string {
	if err == nil {
		return existing
	}
	if existing == "" {
		return err.Error()
	}
	return existing + "; " + err.Error()
}

func configuredPassthroughHosts(value string) func(string) bool {
	hosts := make(map[string]struct{})
	for _, host := range strings.Split(value, ",") {
		host = strings.ToLower(strings.TrimSpace(host))
		if host != "" {
			hosts[host] = struct{}{}
		}
	}
	if len(hosts) == 0 {
		return nil
	}
	return func(host string) bool {
		host = strings.ToLower(host)
		hostname := host
		if splitHost, _, err := net.SplitHostPort(host); err == nil {
			hostname = splitHost
		}
		_, exact := hosts[host]
		_, withoutPort := hosts[hostname]
		return exact || withoutPort
	}
}

type agentControlPlaneRule struct {
	method            string
	host              string
	path              string
	singlePathSegment bool
}

func configuredAgentControlPlane(command []string) func(*http.Request) bool {
	if len(command) == 0 {
		return nil
	}
	var rules []agentControlPlaneRule
	switch strings.ToLower(filepath.Base(command[0])) {
	case "claude":
		rules = []agentControlPlaneRule{
			{method: http.MethodPost, host: "api.anthropic.com", path: "/v1/messages"},
			{method: http.MethodPost, host: "api.anthropic.com", path: "/api/event_logging/v2/batch"},
			{
				method: http.MethodPost, host: "api.anthropic.com", path: "/api/eval/",
				singlePathSegment: true,
			},
			{
				method: http.MethodPost, host: "http-intake.logs.us5.datadoghq.com",
				path: "/api/v2/logs",
			},
			{
				method: http.MethodPost, host: "browser-intake-us5-datadoghq.com",
				path: "/api/v2/logs",
			},
		}
	case "codex":
		rules = []agentControlPlaneRule{
			{method: http.MethodPost, host: "api.openai.com", path: "/v1/responses"},
			{method: http.MethodPost, host: "chatgpt.com", path: "/backend-api/codex/responses"},
			{method: http.MethodGet, host: "chatgpt.com", path: "/backend-api/codex/responses"},
			{method: http.MethodPost, host: "ab.chatgpt.com", path: "/otlp/v1/metrics"},
		}
	case "opencode":
		rules = []agentControlPlaneRule{
			{method: http.MethodPost, host: "opencode.ai", path: "/zen/v1/responses"},
			{method: http.MethodPost, host: "opencode.ai", path: "/zen/v1/chat/completions"},
			{method: http.MethodPost, host: "api.anthropic.com", path: "/v1/messages"},
			{method: http.MethodPost, host: "api.openai.com", path: "/v1/responses"},
			{method: http.MethodPost, host: "api.openai.com", path: "/v1/chat/completions"},
		}
	default:
		return nil
	}
	return func(request *http.Request) bool {
		if request == nil || request.URL == nil {
			return false
		}
		host := strings.ToLower(request.URL.Hostname())
		for _, rule := range rules {
			pathMatches := request.URL.Path == rule.path
			if rule.singlePathSegment && strings.HasPrefix(request.URL.Path, rule.path) {
				suffix := strings.TrimPrefix(request.URL.Path, rule.path)
				pathMatches = suffix != "" && !strings.Contains(suffix, "/")
			}
			if request.Method == rule.method && host == rule.host && pathMatches {
				return true
			}
		}
		return false
	}
}

func loadAdapters(value string) (*adapter.Set, error) {
	var filenames []string
	for _, filename := range strings.Split(value, string(os.PathListSeparator)) {
		if filename = strings.TrimSpace(filename); filename != "" {
			filenames = append(filenames, filename)
		}
	}
	userSources, err := adapter.ReadFiles(filenames)
	if err != nil {
		return nil, err
	}
	builtinSources, err := adapter.BuiltinSources()
	if err != nil {
		return nil, err
	}
	// Both categories deliberately enter the exact same loader call. Load has
	// no knowledge of names, origin, or built-in status.
	sources := append(userSources, builtinSources...)
	return adapter.Load(sources...)
}

func updateHTTPSAudit(record *audit.Record, summary httpsproxy.Summary) {
	record.HTTPS = summary
	unintercepted := make([]audit.Unintercepted, 0,
		len(record.Postgres.Unintercepted)+len(summary.Unintercepted))
	for _, item := range record.Postgres.Unintercepted {
		unintercepted = append(unintercepted, audit.Unintercepted{
			Kind: "postgres", Statement: item.Statement,
			Detail: item.Detail, Time: time.Now().UTC(),
		})
	}
	for _, item := range summary.Unintercepted {
		unintercepted = append(unintercepted, audit.Unintercepted{
			Kind: "https", Host: item.Host, Detail: item.Detail, Time: item.Time,
		})
	}
	record.Unintercepted = unintercepted
}

func stagedAuditUpdater(session *audit.Session) func(httpsproxy.Summary) error {
	return func(summary httpsproxy.Summary) error {
		return session.Update(func(record *audit.Record) {
			updateHTTPSAudit(record, summary)
		})
	}
}

func auditDecision(decision pgproxy.Decision) string {
	if decision == pgproxy.DecisionCommit {
		return "commit"
	}
	return "discard"
}

func logCommand(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("unring log", flag.ContinueOnError)
	flags.SetOutput(stderr)
	asJSON := flags.Bool("json", false, "print structured JSON")
	flags.Usage = func() {
		fmt.Fprintln(stderr, "Usage: unring log [--json] [session-id]")
	}
	if err := flags.Parse(args); err != nil {
		return usageExitCode
	}
	if flags.NArg() > 1 {
		flags.Usage()
		return usageExitCode
	}
	store, err := audit.OpenStore()
	if err != nil {
		fmt.Fprintf(stderr, "unring: open audit log: %v\n", err)
		return internalErrorExitCode
	}
	if flags.NArg() == 0 {
		records, err := store.List()
		if err != nil {
			if records == nil {
				fmt.Fprintf(stderr, "unring: %v\n", err)
				return internalErrorExitCode
			}
			fmt.Fprintf(stderr,
				"unring: warning: some audit records could not be read and were skipped: %v\n",
				err,
			)
		}
		if *asJSON {
			return writeJSON(stdout, stderr, records)
		}
		if len(records) == 0 {
			fmt.Fprintln(stdout, "No unring sessions have been recorded.")
			return 0
		}
		fmt.Fprintln(stdout, "SESSION ID                                  STARTED               OUTCOME      COMMAND")
		for _, record := range records {
			fmt.Fprintf(stdout, "%-43s %-21s %-12s %s\n",
				record.ID, record.StartedAt.Local().Format("2006-01-02 15:04:05"),
				record.Outcome, strings.Join(record.Command, " "))
		}
		return 0
	}
	record, err := store.Load(flags.Arg(0))
	if err != nil {
		fmt.Fprintf(stderr, "unring: %v\n", err)
		return internalErrorExitCode
	}
	record.Files = loadStoredFileSummary(store.StateDir(), record)
	if *asJSON {
		return writeJSON(stdout, stderr, record)
	}
	printAuditRecord(stdout, record)
	return 0
}

func writeJSON(stdout, stderr io.Writer, value any) int {
	encoder := json.NewEncoder(stdout)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(value); err != nil {
		fmt.Fprintf(stderr, "unring: encode audit log: %v\n", err)
		return internalErrorExitCode
	}
	return 0
}

func restoreCommand(args []string, stdout, stderr io.Writer) int {
	force := false
	restoreAll := false
	includeAgentState := false
	var sessionID string
	var selections []string
	for index := 0; index < len(args); index++ {
		switch args[index] {
		case "--force":
			force = true
		case "--all":
			restoreAll = true
		case "--include-agent-state":
			includeAgentState = true
		case "--path":
			index++
			if index >= len(args) {
				fmt.Fprintln(stderr, "unring: --path requires a value")
				return usageExitCode
			}
			selections = append(selections, args[index])
		case "-h", "--help":
			fmt.Fprintln(stdout, "Usage: unring restore [--force] [--all [--include-agent-state]] <session-id> [changed-path ...]")
			fmt.Fprintln(stdout, "With no changed paths, list the files available to restore.")
			return 0
		case "--":
			selections = append(selections, args[index+1:]...)
			index = len(args)
		default:
			if strings.HasPrefix(args[index], "-") {
				fmt.Fprintf(stderr, "unring: unknown restore option %q\n", args[index])
				return usageExitCode
			}
			if sessionID == "" {
				sessionID = args[index]
			} else {
				selections = append(selections, args[index])
			}
		}
	}
	if sessionID == "" {
		fmt.Fprintln(stderr, "Usage: unring restore [--force] [--all [--include-agent-state]] <session-id> [changed-path ...]")
		return usageExitCode
	}
	if includeAgentState && !restoreAll {
		fmt.Fprintln(stderr, "unring: --include-agent-state requires --all")
		return usageExitCode
	}
	store, err := audit.OpenStore()
	if err != nil {
		fmt.Fprintf(stderr, "unring: open audit log: %v\n", err)
		return internalErrorExitCode
	}
	record, err := store.Load(sessionID)
	if err != nil {
		fmt.Fprintf(stderr, "unring: %v\n", err)
		return internalErrorExitCode
	}
	record.Files = loadStoredFileSummary(store.StateDir(), record)
	if len(record.Files.Changes) == 0 {
		fmt.Fprintf(stdout, "Session %s changed no watched files.\n", record.ID)
		printStoredChangeListLimitation(stdout, record.Files, "", true)
		return 0
	}
	if len(selections) == 0 && !restoreAll {
		printRestoreListing(stdout, record)
		return 0
	}
	printStoredChangeListLimitation(stdout, record.Files, "", true)
	if restoreAll {
		if len(selections) > 0 {
			fmt.Fprintln(stderr, "unring: --all cannot be combined with selected paths")
			return usageExitCode
		}
		var skippedAgentState []localrollback.Change
		for _, change := range record.Files.Changes {
			if !includeAgentState && isAgentStateChange(change) {
				skippedAgentState = append(skippedAgentState, change)
				continue
			}
			selections = append(selections, change.Path)
		}
		if len(skippedAgentState) > 0 {
			fmt.Fprintln(stdout, "Skipped agent own-state paths; restore --all excludes them by default:")
			for _, change := range skippedAgentState {
				fmt.Fprintf(stdout, "  skipped  %s\n", change.Path)
			}
			fmt.Fprintf(stdout, "Include them with: unring restore --all --include-agent-state %s\n", record.ID)
		}
	}
	selectedChanges, err := localrollback.ChangesForRestore(record.Files.Changes, selections)
	if err != nil {
		fmt.Fprintf(stderr, "unring: restore %s: %v\n", record.ID, err)
		return internalErrorExitCode
	}
	rootWarningPrinted := false
	for _, change := range selectedChanges {
		if change.RestoreSource != localrollback.RestoreSourceVolume {
			continue
		}
		if !rootWarningPrinted {
			fmt.Fprintln(stderr, "unring: ROOT PRIVILEGES REQUIRED FOR SNAPSHOT-ONLY RESTORE")
			fmt.Fprintln(stderr, "unring: This path was outside the clone scope. macOS permits only root to mount the recorded read-only APFS snapshot, so sudo will ask for authorization before unring accesses its prior contents.")
			rootWarningPrinted = true
		}
		fmt.Fprintf(stderr, "unring: snapshot-only path: %s\n", change.Path)
	}
	results, err := localrollback.RestoreRecorded(store.StateDir(), record.ID, record.Files, selections, force)
	if err != nil {
		fmt.Fprintf(stderr, "unring: restore %s: %v\n", record.ID, err)
		return internalErrorExitCode
	}
	exitCode := 0
	for _, result := range results {
		restoreEvent := localrollback.RestoreRecord{
			Path: result.Path, Status: result.Status, Sidecar: result.Sidecar, RestoredAt: time.Now().UTC(),
		}
		switch result.Status {
		case "restored":
			fmt.Fprintf(stdout, "restored  %s\n", result.Path)
		case "already-restored":
			fmt.Fprintf(stdout, "already restored  %s\n", result.Path)
		case "refused":
			exitCode = internalErrorExitCode
			if result.Err != nil {
				restoreEvent.Error = result.Err.Error()
				fmt.Fprintf(stderr, "refused   %s: %v; not restored\n", result.Path, result.Err)
				break
			}
			fmt.Fprintf(stderr, "refused   %s: changed after the session ended; not overwritten\n", result.Path)
			if result.Sidecar != "" {
				fmt.Fprintf(stderr, "snapshot version written alongside: %s\n", result.Sidecar)
			} else {
				fmt.Fprintln(stderr, "pre-session state was absence, so there is no snapshot file to write alongside")
			}
			fmt.Fprintln(stderr, "rerun with --force to overwrite the conflict explicitly")
		case "unavailable":
			exitCode = internalErrorExitCode
			if result.Err != nil {
				restoreEvent.Error = result.Err.Error()
				fmt.Fprintf(stderr, "unavailable %s: %v\n", result.Path, result.Err)
			} else {
				fmt.Fprintf(stderr, "unavailable %s: path was outside snapshot coverage\n", result.Path)
			}
		default:
			exitCode = internalErrorExitCode
			restoreEvent.Status = "error"
			if result.Err != nil {
				restoreEvent.Error = result.Err.Error()
				fmt.Fprintf(stderr, "error     %s: %v; not restored\n", result.Path, result.Err)
			} else {
				fmt.Fprintf(stderr, "error     %s; not restored\n", result.Path)
			}
		}
		record.Files.RestoreEvents = append(record.Files.RestoreEvents, restoreEvent)
	}
	if err := store.Save(record); err != nil {
		fmt.Fprintf(stderr, "unring: record restore outcome: %v\n", err)
		return internalErrorExitCode
	}
	return exitCode
}

func snapshotsCommand(args []string, stdout, stderr io.Writer) int {
	if len(args) != 0 {
		fmt.Fprintln(stderr, "Usage: unring snapshots")
		return usageExitCode
	}
	store, err := audit.OpenStore()
	if err != nil {
		fmt.Fprintf(stderr, "unring: open state store: %v\n", err)
		return internalErrorExitCode
	}
	capBytes, err := localrollback.RetentionCapForState(store.StateDir())
	if err != nil {
		fmt.Fprintf(stderr, "unring: %v\n", err)
		return usageExitCode
	}
	usage, err := localrollback.StorageUsage(store.StateDir(), capBytes)
	if err != nil {
		fmt.Fprintf(stderr, "unring: inspect snapshot storage: %v\n", err)
		return internalErrorExitCode
	}
	if usage.Exact {
		fmt.Fprintf(stdout, "Snapshot storage: %d bytes used of %d bytes; %d sessions retained.\n",
			usage.Bytes, usage.CapBytes, usage.Sessions)
	} else {
		fmt.Fprintf(stdout, "Snapshot storage: upper-bound estimate %d bytes of %d bytes; %d sessions retained.\n",
			usage.Bytes, usage.CapBytes, usage.Sessions)
	}
	records, listErr := store.List()
	if listErr != nil {
		fmt.Fprintf(stderr, "unring: inspect session backstops: %v\n", listErr)
	}
	for _, record := range records {
		backstop := record.Files.Backstop
		if !backstop.Available {
			reason := backstop.Reason
			if reason == "" {
				reason = "no backstop was recorded"
			}
			fmt.Fprintf(stdout, "%s  NO WHOLE-VOLUME BACKSTOP — %s\n", record.ID, reason)
			continue
		}
		presences := localrollback.InspectBackstop(backstop)
		for _, presence := range presences {
			status := "present"
			if presence.Error != "" {
				status = "presence unknown: " + presence.Error
			} else if !presence.Present {
				status = "PURGED OR DELETED"
			}
			fmt.Fprintf(stdout, "%s  %s on %s — %s\n",
				record.ID, presence.Snapshot.Name, presence.Snapshot.MountPoint, status)
		}
	}
	return 0
}

func printRestoreListing(output io.Writer, record audit.Record) {
	fmt.Fprintf(output, "UNRING FILE CHANGES %s\n", record.ID)
	printStoredChangeListLimitation(output, record.Files, "  ", true)
	regular, agentState := groupAgentStateChanges(record.Files.Changes)
	for _, change := range regular {
		fmt.Fprintf(output, "  %-8s %s\n", change.Kind, change.Path)
		if change.RestoreSource == localrollback.RestoreSourceVolume {
			fmt.Fprintln(output, "             SNAPSHOT ONLY: restore requires sudo because APFS snapshot mounting is root-only")
		} else if change.UnrestorableReason != "" {
			fmt.Fprintf(output, "             NOT RESTORABLE: %s\n", change.UnrestorableReason)
		}
	}
	if len(agentState) > 0 {
		fmt.Fprintln(output, "AGENT OWN-STATE CHANGES — reported, but skipped by restore --all unless explicitly included")
		for _, change := range agentState {
			fmt.Fprintf(output, "  %-8s %s\n", change.Kind, change.Path)
			if change.RestoreSource == localrollback.RestoreSourceVolume {
				fmt.Fprintln(output, "             SNAPSHOT ONLY: restore requires sudo because APFS snapshot mounting is root-only")
			} else if change.UnrestorableReason != "" {
				fmt.Fprintf(output, "             NOT RESTORABLE: %s\n", change.UnrestorableReason)
			}
		}
		fmt.Fprintf(output, "Include this group with: unring restore --all --include-agent-state %s\n", record.ID)
	}
	if !record.Files.Retained {
		if hasVolumeRestorableFileChange(record.Files.Changes) {
			fmt.Fprintln(output, "Clone snapshot data has been evicted; SNAPSHOT ONLY paths remain restorable while their APFS snapshot is present.")
			fmt.Fprintf(output, "Restore selected snapshot-only paths with: unring restore %s <path> [...]\n", record.ID)
			return
		}
		fmt.Fprintln(output, "Clone snapshot data has been evicted; these changes are no longer restorable.")
		return
	}
	if hasRestorableFileChange(record.Files.Changes) {
		fmt.Fprintf(output, "Restore selected paths with: unring restore %s <path> [...]\n", record.ID)
		fmt.Fprintf(output, "Restore every restorable path except agent own-state with: unring restore --all %s\n", record.ID)
	} else {
		fmt.Fprintln(output, "No changed path has restorable snapshot data.")
	}
}

func loadStoredFileSummary(stateDir string, record audit.Record) localrollback.Summary {
	manifestSummary, err := localrollback.LoadSealedSummary(stateDir, record.ID)
	if err != nil {
		// Clone retention can evict the manifest before the audit record. The
		// audit copy also remains authoritative when the manifest is unsealed.
		return record.Files
	}
	summary := record.Files
	// The audit record is the later, more durable copy of changes and errors.
	// Only the manifest's persisted disclosure fields are needed here.
	summary.ChangeListScope = manifestSummary.ChangeListScope
	summary.ChangeListRoots = append([]string(nil), manifestSummary.ChangeListRoots...)
	return summary
}

func hasRestorableFileChange(changes []localrollback.Change) bool {
	for _, change := range changes {
		if change.UnrestorableReason == "" {
			return true
		}
	}
	return false
}

func hasVolumeRestorableFileChange(changes []localrollback.Change) bool {
	for _, change := range changes {
		if change.UnrestorableReason == "" && change.RestoreSource == localrollback.RestoreSourceVolume {
			return true
		}
	}
	return false
}

func printAuditRecord(output io.Writer, record audit.Record) {
	fmt.Fprintf(output, "UNRING SESSION %s\n", record.ID)
	fmt.Fprintf(output, "Started:  %s\n", record.StartedAt.Local().Format(time.RFC3339))
	if !record.EndedAt.IsZero() {
		fmt.Fprintf(output, "Ended:    %s\n", record.EndedAt.Local().Format(time.RFC3339))
	}
	fmt.Fprintf(output, "Command:  %s\n", strings.Join(record.Command, " "))
	fmt.Fprintf(output, "Decision: %s\n", record.Decision)
	fmt.Fprintf(output, "Outcome:  %s\n", record.Outcome)
	fmt.Fprintf(output, "Exit code: %d\n", record.ExitCode)
	fmt.Fprintf(output, "Outbound interception: %t\n", record.Outbound)
	if record.Error != "" {
		fmt.Fprintf(output, "Error: %s\n", record.Error)
	}
	printAuditFiles(output, record.Files)
	if hasOnlyObservedHTTPSActivity(record.Postgres, record.HTTPS, record.GH) {
		printObservedSummaryWithExternal(output, record.Postgres, record.HTTPS, record.GH)
	} else {
		printSummaryWithExternal(output, record.Postgres, record.HTTPS, record.GH)
	}
	if len(record.Approvals) > 0 {
		fmt.Fprintln(output, "\nIRREVERSIBLE ACTION DECISIONS")
		for _, approval := range record.Approvals {
			fmt.Fprintf(output, "  - [%s] %s\n", approval.Decision, compactSQL(approval.Statement))
			fmt.Fprintf(output, "    Reason: %s\n", approval.Reason)
			if approval.Error != "" {
				fmt.Fprintf(output, "    Error: %s\n", approval.Error)
			}
		}
	}
}

func isNamedAlias(name string) bool {
	switch name {
	case "claude", "codex", "opencode":
		return true
	default:
		return false
	}
}

func resolvesOnPath(name string) bool {
	_, err := exec.LookPath(name)
	return err == nil
}

func parseBackendConfig() (*pgconn.Config, error) {
	connectionString := os.Getenv("DATABASE_URL")
	if strings.TrimSpace(connectionString) == "" {
		return nil, errors.New(
			"DATABASE_URL is not set; point it at the real PostgreSQL database, for example " +
				"postgresql://user:password@localhost/database",
		)
	}
	config, err := pgconn.ParseConfig(connectionString)
	if err != nil {
		return nil, fmt.Errorf("read real Postgres connection settings: %w", err)
	}
	return config, nil
}

func parseOptionalBackendConfig() (*pgconn.Config, bool, error) {
	if strings.TrimSpace(os.Getenv("DATABASE_URL")) == "" {
		return nil, false, nil
	}
	config, err := parseBackendConfig()
	if err != nil {
		return nil, true, err
	}
	return config, true, nil
}

func promptDecision(input io.Reader, output io.Writer) pgproxy.Decision {
	if !isTerminal(input) {
		fmt.Fprintln(output, "No interactive terminal; defaulting to discard. Use --commit to commit.")
		return pgproxy.DecisionRollback
	}

	reader := bufio.NewReader(input)
	for {
		fmt.Fprint(output, "Commit or discard? [c/D] ")
		answer, err := reader.ReadString('\n')
		if err != nil && !errors.Is(err, io.EOF) {
			fmt.Fprintf(output, "\nCould not read decision (%v); defaulting to discard.\n", err)
			return pgproxy.DecisionRollback
		}
		switch strings.ToLower(strings.TrimSpace(answer)) {
		case "c", "commit":
			return pgproxy.DecisionCommit
		case "", "d", "discard":
			return pgproxy.DecisionRollback
		default:
			fmt.Fprintln(output, "Please enter commit or discard.")
		}
		if errors.Is(err, io.EOF) {
			return pgproxy.DecisionRollback
		}
	}
}

func promptIrreversibleApproval(
	input io.Reader,
	output io.Writer,
	request pgproxy.ApprovalRequest,
) bool {
	fmt.Fprintln(output, "\nIrreversible PostgreSQL action requested")
	fmt.Fprintln(output, "  SQL (exactly as requested):")
	fmt.Fprintln(output, request.SQL)
	fmt.Fprintf(output, "  Reason: %s.\n", request.Reason)
	fmt.Fprintln(output,
		"  This will run now on a separate non-transactional connection and cannot be undone by discard.")
	if !isTerminal(input) || !isTerminalWriter(output) {
		fmt.Fprintln(output, "  No interactive terminal; declining the action.")
		return false
	}

	fmt.Fprint(output, "Run this irreversible action? [y/N] ")
	answer, err := readOnePromptLine(input)
	if err != nil && !errors.Is(err, io.EOF) {
		fmt.Fprintf(output, "\nCould not read approval (%v); declining.\n", err)
		return false
	}
	approved := strings.EqualFold(strings.TrimSpace(answer), "y") ||
		strings.EqualFold(strings.TrimSpace(answer), "yes")
	if !approved {
		fmt.Fprintln(output, "Action declined; it was not run.")
	}
	return approved
}

func promptHTTPSApproval(
	input io.Reader,
	output io.Writer,
	request httpsproxy.ApprovalRequest,
) bool {
	fmt.Fprintln(output, "\nHTTPS action needs approval")
	fmt.Fprintf(output, "  Request: %s %s\n", request.Method, request.URL)
	if request.Adapter != "" {
		fmt.Fprintf(output, "  Adapter rule: %s / %s\n", request.Adapter, request.Rule)
	}
	fmt.Fprintf(output, "  Reason: %s.\n", request.Reason)
	fmt.Fprintln(output,
		"  Approving sends this request to the real service now; declining guarantees it is not sent.")
	if !isTerminal(input) || !isTerminalWriter(output) {
		fmt.Fprintln(output, "  No interactive terminal; declining the action.")
		return false
	}
	fmt.Fprint(output, "Send this HTTPS request? [y/N] ")
	answer, err := readOnePromptLine(input)
	if err != nil && !errors.Is(err, io.EOF) {
		fmt.Fprintf(output, "\nCould not read approval (%v); declining.\n", err)
		return false
	}
	approved := strings.EqualFold(strings.TrimSpace(answer), "y") ||
		strings.EqualFold(strings.TrimSpace(answer), "yes")
	if !approved {
		fmt.Fprintln(output, "Action declined; it was not sent.")
	}
	return approved
}

func promptGHApproval(
	input io.Reader,
	output io.Writer,
	request ghshim.ApprovalRequest,
) bool {
	fmt.Fprintln(output, "\ngh action needs approval")
	fmt.Fprintf(output, "  Invocation: %s\n", request.Invocation)
	fmt.Fprintf(output, "  Structured intent: %s\n", request.Intent)
	fmt.Fprintf(output, "  Reason: %s.\n", request.Reason)
	fmt.Fprintln(output,
		"  Approving runs the real gh now with the same stdin, stdout, stderr, and terminal; declining guarantees it is not run.")
	if !isTerminal(input) || !isTerminalWriter(output) {
		fmt.Fprintln(output, "  No interactive terminal; declining the action.")
		return false
	}
	fmt.Fprint(output, "Run this gh invocation? [y/N] ")
	answer, err := readOnePromptLine(input)
	if err != nil && !errors.Is(err, io.EOF) {
		fmt.Fprintf(output, "\nCould not read approval (%v); declining.\n", err)
		return false
	}
	approved := strings.EqualFold(strings.TrimSpace(answer), "y") ||
		strings.EqualFold(strings.TrimSpace(answer), "yes")
	if !approved {
		fmt.Fprintln(output, "Action declined; gh was not run.")
	}
	return approved
}

// readOnePromptLine deliberately limits every Read call to one byte. A
// bufio.Reader may read several canonical terminal lines at once on Linux;
// discarding that reader after the approval would then swallow input intended
// for the resumed child or the final review prompt.
func readOnePromptLine(input io.Reader) (string, error) {
	var line strings.Builder
	var buffer [1]byte
	for {
		count, err := input.Read(buffer[:])
		if count == 1 {
			line.WriteByte(buffer[0])
			if buffer[0] == '\n' {
				return line.String(), nil
			}
		}
		if err != nil {
			return line.String(), err
		}
		if count == 0 {
			return line.String(), io.ErrNoProgress
		}
	}
}

func promptDecisionWithSignal(
	input io.Reader,
	output io.Writer,
	signals <-chan os.Signal,
) (pgproxy.Decision, bool) {
	if !isTerminal(input) || !isTerminalWriter(output) {
		fmt.Fprintln(output, "No interactive terminal; defaulting to discard. Use --commit to commit.")
		return pgproxy.DecisionRollback, false
	}

	decision := make(chan pgproxy.Decision, 1)
	go func() {
		decision <- promptDecision(input, output)
	}()
	select {
	case chosen := <-decision:
		return chosen, false
	case <-signals:
		fmt.Fprintln(output, "\nSignal received: discarding the session.")
		return pgproxy.DecisionRollback, true
	}
}

func pendingSignal(signals <-chan os.Signal) bool {
	select {
	case <-signals:
		return true
	default:
		return false
	}
}

func printSummary(output io.Writer, summary pgproxy.Summary) {
	printSummaryWithHTTPS(output, summary, httpsproxy.Summary{Sealed: true})
}

func printSummaryWithHTTPS(
	output io.Writer,
	summary pgproxy.Summary,
	httpsSummary httpsproxy.Summary,
) {
	printSummaryWithExternal(output, summary, httpsSummary, ghshim.Summary{Sealed: true})
}

func printSummaryWithExternal(
	output io.Writer,
	summary pgproxy.Summary,
	httpsSummary httpsproxy.Summary,
	ghSummary ghshim.Summary,
) {
	printSummaryWithExternalDecision(output, summary, httpsSummary, ghSummary, true)
}

func printObservedSummaryWithExternal(
	output io.Writer,
	summary pgproxy.Summary,
	httpsSummary httpsproxy.Summary,
	ghSummary ghshim.Summary,
) {
	printSummaryWithExternalDecision(output, summary, httpsSummary, ghSummary, false)
}

func hasOnlyObservedHTTPSActivity(
	summary pgproxy.Summary,
	httpsSummary httpsproxy.Summary,
	ghSummary ghshim.Summary,
) bool {
	return len(httpsSummary.Requests) > 0 &&
		!summary.HasReviewableActivity() &&
		!httpsSummary.HasReviewableActivity() &&
		!ghSummary.HasReviewableActivity()
}

func printSummaryWithExternalDecision(
	output io.Writer,
	summary pgproxy.Summary,
	httpsSummary httpsproxy.Summary,
	ghSummary ghshim.Summary,
	decisionRequired bool,
) {
	failed := 0
	for _, query := range summary.Queries {
		if query.Failed {
			failed++
		}
	}

	fmt.Fprintln(output, "\nUNRING SESSION REVIEW")
	if decisionRequired {
		fmt.Fprintln(output, "One decision applies to the whole session; partial commit is not available.")
		fmt.Fprintln(output,
			"Keeping database changes while withholding a related external action could commit inconsistent state—for example, notified_at set when its mail was never sent.")
	} else {
		fmt.Fprintln(output,
			"No commit/discard decision was needed; only observed HTTPS traffic is shown.")
	}
	if !summary.FullyReversible || httpsSummary.HasForwardedEffects() || ghMayHaveExternalEffect(ghSummary) {
		fmt.Fprintln(output, "!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!")
		fmt.Fprintln(output, "WARNING: THIS SESSION IS NOT FULLY REVERSIBLE")
		fmt.Fprintln(output, "Unring cannot guarantee every recorded effect can be undone by discarding.")
		fmt.Fprintln(output, "!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!")
	}
	printStructuralBlindSpots(output, defaultReviewWidth)
	writeChangeSummary(output, summary)
	fmt.Fprintln(output, "\nSTATEMENTS")
	if summary.InterceptionStatus == pgproxy.InterceptionNotConfigured {
		fmt.Fprintln(output,
			"  PostgreSQL statements were not intercepted and cannot appear in this review.")
	} else {
		fmt.Fprintf(output, "  Connections: %d (one shared backend transaction)\n", summary.Connections)
		fmt.Fprintf(output, "  Query batches: %d", len(summary.Queries))
		if failed > 0 {
			fmt.Fprintf(output, " (%d failed)", failed)
		}
		fmt.Fprintln(output)

		for _, query := range summary.Queries {
			status := "ok"
			if query.Failed {
				status = "error"
			}
			fmt.Fprintf(output, "  - [%s] %s", status, compactSQL(query.SQL))
			if len(query.CommandTags) > 0 {
				fmt.Fprintf(output, " -> %s", strings.Join(query.CommandTags, ", "))
			}
			fmt.Fprintln(output)
			if query.Error != "" {
				fmt.Fprintf(output, "    Error: %s\n", query.Error)
			}
		}
	}
	if len(summary.NonTransactional) > 0 {
		fmt.Fprintln(output, "\nNON-TRANSACTIONAL EFFECTS — DISCARD CANNOT UNDO THESE")
		for _, effect := range summary.NonTransactional {
			fmt.Fprintf(output, "  - %s\n", effect.Detail)
		}
	}
	if len(summary.IrreversibleActions) > 0 {
		fmt.Fprintln(output, "\nAPPROVED IRREVERSIBLE ACTIONS — DISCARD CANNOT UNDO THESE")
		fmt.Fprintln(output, "  Successful actions ran outside the shared transaction; discard cannot undo them.")
		for _, action := range summary.IrreversibleActions {
			fmt.Fprintf(output, "  - %s", compactSQL(action.SQL))
			if len(action.CommandTags) > 0 {
				fmt.Fprintf(output, " -> %s", strings.Join(action.CommandTags, ", "))
			}
			fmt.Fprintln(output)
			if action.Error != "" {
				fmt.Fprintf(output, "    Error: %s\n", action.Error)
			}
		}
	}
	if len(httpsSummary.Staged) > 0 {
		pending := false
		for _, request := range httpsSummary.Staged {
			if request.State == "" || request.State == "pending" {
				pending = true
				break
			}
		}
		if pending {
			fmt.Fprintln(output, "\nPENDING HTTPS — WILL BE SENT IF YOU COMMIT")
			fmt.Fprintln(output,
				"  These requests have not reached their origins. Discard drops them without sending.")
		} else {
			fmt.Fprintln(output, "\nSTAGED HTTPS CALLS — FINAL OUTCOME")
		}
		for _, request := range httpsSummary.Staged {
			fmt.Fprintf(output, "  - [%s] %s %s\n", request.State, request.Method, request.URL)
			fmt.Fprintf(output, "    Idempotency key: %s\n", request.IdempotencyKey)
			if request.Error != "" {
				fmt.Fprintf(output, "    Error: %s\n", request.Error)
			}
			if request.Warning != "" {
				fmt.Fprintf(output, "    Warning: %s\n", request.Warning)
			}
			if request.State != "" && request.State != "pending" &&
				request.State != "discarded" {
				printUndoDisclosure(output, request.Undo)
			}
		}
	}
	declinedApprovals := 0
	for _, approval := range httpsSummary.Approvals {
		if approval.Decision != "approved" {
			declinedApprovals++
		}
	}
	if declinedApprovals > 0 {
		fmt.Fprintln(output, "\nHTTPS APPROVALS — NOT SENT")
		fmt.Fprintln(output, "  These needs-approval requests did not reach their origins.")
		for _, approval := range httpsSummary.Approvals {
			if approval.Decision == "approved" {
				continue
			}
			fmt.Fprintf(output, "  - [%s] %s %s\n",
				approval.Decision, approval.Method, approval.URL)
			if approval.Error != "" {
				fmt.Fprintf(output, "    Error: %s\n", approval.Error)
			}
		}
	}
	printForwardedHTTPSRequests(output, httpsSummary.Requests,
		httpsproxy.RequestDispositionControlPlane,
		"AGENT CONTROL PLANE — FORWARDED WITHOUT GATING",
		"  These enumerated agent operational requests were deliberately not gated so the wrapped agent could function; they remain visible here and in the audit.")
	printForwardedHTTPSRequests(output, httpsSummary.Requests,
		httpsproxy.RequestDispositionSafeRead,
		"HTTPS SAFE READS — OBSERVED AND FORWARDED",
		"  These safe-method requests were recorded for audit and did not create an irreversible-effect warning.")
	printForwardedHTTPSRequests(output, httpsSummary.Requests, "",
		"HTTPS REQUESTS — INTERCEPTED AND ALREADY FORWARDED",
		"  These requests reached their destinations; discard may not undo their effects.")
	if len(ghSummary.Records) > 0 {
		fmt.Fprintln(output, "\nGH INVOCATIONS — MUTATIONS AND AMBIGUOUS COMMANDS")
		printGHSummary(output, ghSummary)
	}
	if len(summary.Unintercepted) > 0 || len(httpsSummary.Unintercepted) > 0 {
		fmt.Fprintln(output, "\n================================================================")
		fmt.Fprintln(output, "!!! UN-INTERCEPTED OR UNCLASSIFIED TRAFFIC !!!")
		fmt.Fprintln(output, "Coverage is incomplete. These items are not part of the normal statement list.")
		for _, item := range summary.Unintercepted {
			if item.Statement != "" {
				fmt.Fprintf(output, "  Statement: %s\n", item.Statement)
			}
			fmt.Fprintf(output, "  Detail: %s\n", item.Detail)
		}
		for _, item := range httpsSummary.Unintercepted {
			if item.Host != "" {
				fmt.Fprintf(output, "  Host: %s\n", item.Host)
			}
			fmt.Fprintf(output, "  Detail: %s\n", item.Detail)
		}
		fmt.Fprintln(output, "================================================================")
	}
}

func printForwardedHTTPSRequests(
	output io.Writer,
	requests []httpsproxy.RequestRecord,
	disposition string,
	header string,
	detail string,
) {
	matching := make([]httpsproxy.RequestRecord, 0, len(requests))
	for _, request := range requests {
		if disposition == "" {
			if request.Disposition == httpsproxy.RequestDispositionSafeRead ||
				request.Disposition == httpsproxy.RequestDispositionControlPlane {
				continue
			}
		} else if request.Disposition != disposition {
			continue
		}
		matching = append(matching, request)
	}
	if len(matching) == 0 {
		return
	}
	fmt.Fprintln(output, "\n"+header)
	fmt.Fprintln(output, detail)
	for _, request := range matching {
		status := "forwarded"
		if request.StatusCode != 0 {
			status = fmt.Sprintf("HTTP %d", request.StatusCode)
		}
		if request.Error != "" {
			status = "forwarding failed: " + request.Error
		}
		fmt.Fprintf(output, "  - [%s] %s %s\n", status, request.Method, request.URL)
		if request.Error != "" {
			fmt.Fprintf(output, "    Error: %s\n", request.Error)
		}
		if request.Disposition != httpsproxy.RequestDispositionSafeRead &&
			request.Disposition != httpsproxy.RequestDispositionControlPlane {
			printUndoDisclosure(output, request.Undo)
		}
	}
}

func printGHSummary(output io.Writer, summary ghshim.Summary) {
	for _, record := range summary.Records {
		fmt.Fprintf(output, "  - [%s] gh %s\n",
			record.State, strings.Join(record.Arguments, " "))
		fmt.Fprintf(output, "    Intent: %s\n", record.Intent)
		fmt.Fprintf(output, "    Reason: %s\n", record.Reason)
		if record.UndoEffect != "" {
			fmt.Fprintf(output, "    Declared compensation: %s\n", record.UndoEffect)
		}
		if record.StillExists != "" {
			fmt.Fprintf(output, "    What remains or may remain: %s\n", record.StillExists)
		}
		if record.UndoState != "" {
			fmt.Fprintf(output, "    Compensation state: %s\n", record.UndoState)
		}
		if record.UndoError != "" {
			fmt.Fprintf(output, "    Compensation error: %s\n", record.UndoError)
		}
		if record.Error != "" {
			fmt.Fprintf(output, "    Error: %s\n", record.Error)
		}
	}
}

func printUndoDisclosure(output io.Writer, undo *httpsproxy.UndoRecord) {
	if undo == nil {
		fmt.Fprintln(output,
			"    Discard cannot undo this forwarded request; any external effect remains.")
		return
	}
	switch undo.State {
	case "available":
		fmt.Fprintf(output, "    Discard will attempt: %s\n", undo.Effect)
		fmt.Fprintf(output, "    What remains or may remain: %s\n", undo.StillExists)
	case "succeeded":
		fmt.Fprintf(output, "    Compensation succeeded: %s\n", undo.Effect)
		fmt.Fprintf(output, "    What still remains: %s\n", undo.StillExists)
	case "failed", "unavailable":
		fmt.Fprintf(output, "    COMPENSATION %s: %s\n",
			strings.ToUpper(undo.State), undo.Error)
		fmt.Fprintf(output, "    WHAT REMAINS: %s\n", undo.StillExists)
	default:
		fmt.Fprintf(output, "    Compensation state: %s; action: %s\n", undo.State, undo.Effect)
		fmt.Fprintf(output, "    Boundary: %s\n", undo.StillExists)
	}
}

func writeChangeSummary(output io.Writer, summary pgproxy.Summary) {
	if summary.InterceptionStatus == pgproxy.InterceptionNotConfigured {
		fmt.Fprintln(output, "\nDATABASE")
		fmt.Fprintln(output,
			"  NOT INTERCEPTED — no database traffic was intercepted because DATABASE_URL was unset or blank.")
		fmt.Fprintln(output,
			"  This is a coverage statement, not evidence that the child did not access a database.")
		return
	}
	fmt.Fprintln(output, "\nDATA CHANGES (reported by PostgreSQL for the sealed transaction)")
	if !summary.Changes.Complete {
		fmt.Fprintf(output, "  UNKNOWN — the change summary is incomplete: %s\n", summary.Changes.Error)
	} else if len(summary.Changes.Rows) == 0 {
		fmt.Fprintln(output, "  No rows inserted, updated, or deleted.")
	} else {
		for _, change := range summary.Changes.Rows {
			fmt.Fprintf(output, "  - %s: %d inserted, %d updated, %d deleted\n",
				change.Table, change.Inserted, change.Updated, change.Deleted)
		}
	}
	fmt.Fprintln(output, "  Note: PostgreSQL sequences do not roll back; discarded sessions can leave ID gaps.")
	fmt.Fprintln(output, "\nSCHEMA CHANGES (sealed catalog comparison)")
	if !summary.Changes.Complete {
		fmt.Fprintln(output, "  UNKNOWN — catalog changes could not be determined safely.")
	} else if len(summary.Changes.Schema) == 0 {
		fmt.Fprintln(output, "  No schema changes.")
	} else {
		for _, change := range summary.Changes.Schema {
			fmt.Fprintf(output, "  - %s %s %s\n", change.Action, change.Kind, change.Object)
		}
	}
}

func printCoverageOnlyReview(output io.Writer, summary pgproxy.Summary) {
	fmt.Fprintln(output, "\nUNRING SESSION REVIEW")
	if summary.InterceptionStatus == pgproxy.InterceptionNotConfigured {
		writeChangeSummary(output, summary)
	}
	printStructuralBlindSpots(output, defaultReviewWidth)
}

func printStructuralBlindSpots(output io.Writer, width int) {
	fmt.Fprintln(output)
	writeWrappedLine(output, "STRUCTURAL BLIND SPOTS — NO RECORD IS POSSIBLE", "", "", width)
	writeWrappedLine(output, "SSH traffic, including git push over SSH.", "  - ", "    ", width)
	writeWrappedLine(output, "direct-to-IP and raw-socket connections.", "  - ", "    ", width)
	writeWrappedLine(output,
		"Clients that ignore proxy or PATH settings leave no record.", "  - ", "    ", width)
	writeWrappedLine(output,
		"Unshimmed Go CLIs such as aws, docker, terraform, and kubectl on macOS.",
		"  - ", "    ", width)
}

func writeWrappedLine(
	output io.Writer,
	text string,
	firstPrefix string,
	continuationPrefix string,
	width int,
) {
	if width <= 0 {
		width = defaultReviewWidth
	}
	line := firstPrefix
	for _, word := range strings.Fields(text) {
		separator := ""
		if line != firstPrefix {
			separator = " "
		}
		candidate := line + separator + word
		if line != firstPrefix && ansi.StringWidth(candidate) > width {
			fmt.Fprintln(output, line)
			line = continuationPrefix + word
			firstPrefix = continuationPrefix
			continue
		}
		line = candidate
	}
	fmt.Fprintln(output, line)
}

func affectedRows(tags []string) string {
	var affected []string
	for _, tag := range tags {
		fields := strings.Fields(tag)
		if len(fields) < 2 {
			continue
		}
		operation := strings.ToUpper(fields[0])
		if operation != "INSERT" && operation != "UPDATE" && operation != "DELETE" &&
			operation != "MERGE" && operation != "COPY" && operation != "MOVE" &&
			operation != "FETCH" && operation != "SELECT" {
			continue
		}
		countText := fields[len(fields)-1]
		if _, err := strconv.ParseInt(countText, 10, 64); err != nil {
			continue
		}
		affected = append(affected, strings.ToLower(operation)+" "+countText)
	}
	return strings.Join(affected, ", ")
}

func compactSQL(sql string) string {
	compacted := strings.Join(strings.Fields(sql), " ")
	const maximum = 160
	if len(compacted) <= maximum {
		return compacted
	}
	return compacted[:maximum-3] + "..."
}

func isTerminal(reader io.Reader) bool {
	file, ok := reader.(*os.File)
	return ok && term.IsTerminal(int(file.Fd()))
}

func pastTense(decision pgproxy.Decision) string {
	if decision == pgproxy.DecisionCommit {
		return "committed"
	}
	return "discarded"
}

func printUsage(output io.Writer) {
	fmt.Fprintln(output, "unring snapshots an agent's local file changes for later per-file restore.")
	fmt.Fprintln(output, "PostgreSQL changes still end with one decision: commit or discard; outbound interception is optional.")
	fmt.Fprintln(output)
	fmt.Fprintln(output, "Primary workflow (one bounded agent task that exits):")
	fmt.Fprintln(output, "  unring run -- claude -p 'Implement one bounded task, then stop'")
	fmt.Fprintln(output, "  unring run -- <one-shot-agent-command> [args...]")
	fmt.Fprintln(output)
	fmt.Fprintln(output, "Usage:")
	fmt.Fprintln(output, "  unring run [--commit | --discard] [--outbound] [--watch path | --watch-only path] -- <command> [args...]")
	fmt.Fprintln(output, "  unring log [--json] [session-id]")
	fmt.Fprintln(output, "  unring restore [--force] [--all [--include-agent-state]] <session-id> [changed-path ...]")
	fmt.Fprintln(output, "  unring snapshots")
	fmt.Fprintln(output, "  unring <command-on-PATH> [--] [args...]")
	fmt.Fprintln(output, "  unring claude|codex|opencode [--] [args...]")
	fmt.Fprintln(output, "  unring --version")
	fmt.Fprintln(output)
	fmt.Fprintln(output, "Optional PostgreSQL coverage:")
	fmt.Fprintln(output, "  export DATABASE_URL='postgresql://user:password@localhost/database'")
	fmt.Fprintln(output, "  unring run -- <one-shot-agent-command>")
	fmt.Fprintln(output)
	fmt.Fprintln(output, "DATABASE_URL may be unset; file snapshots and the audit log still run, while")
	fmt.Fprintln(output, "the review says PostgreSQL was not intercepted. A nonblank DATABASE_URL must")
	fmt.Fprintln(output, "parse and reach PostgreSQL 14 or newer or the child will not start.")
	fmt.Fprintln(output, "Use --outbound to opt into HTTPS interception, adapters, and the gh shim.")
	fmt.Fprintln(output)
	fmt.Fprintln(output, "Keep database-backed runs bounded: the shared transaction remains open for the")
	fmt.Fprintln(output, "whole child lifetime, holding locks and delaying cleanup. Without a terminal,")
	fmt.Fprintln(output, "the safe default is discard; use --commit or --discard for automation.")
}

func versionString() string {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return "unring devel"
	}
	version := info.Main.Version
	if version == "" || version == "(devel)" {
		version = "devel"
	}
	var revision string
	modified := false
	for _, setting := range info.Settings {
		switch setting.Key {
		case "vcs.revision":
			revision = setting.Value
		case "vcs.modified":
			modified = setting.Value == "true"
		}
	}
	if len(revision) > 12 {
		revision = revision[:12]
	}
	if revision != "" {
		version += " (" + revision
		if modified {
			version += ", modified"
		}
		version += ")"
	}
	return "unring " + version
}
