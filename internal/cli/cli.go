// Package cli implements the unring command line.
package cli

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
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
	"sync"
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
	internalErrorExitCode         = 1
	usageExitCode                 = 2
	defaultReviewWidth            = 80
	defaultSessionListLimit       = 50
	completionKindNoDecision      = "no_decision_needed"
	completionKindExplicitDiscard = "explicit_discard"
	completionKindAbnormalDiscard = "abnormal_discard"

	quietSessionDisclosure         = "unring: Files changed: 0; the post-session scan completed. Nothing intercepted. Outbound is not covered unless --outbound was given. Not visible to unring: SSH/git push, raw sockets, unshimmed CLIs."
	quietSessionActivityDisclosure = "unring: nothing intercepted. Outbound is not covered unless --outbound was given. Not visible to unring: SSH/git push, raw sockets, unshimmed CLIs."
	homeScanExclusions             = "Library, node_modules, .git, .cache, and go/pkg"
)

type stringListFlag []string

type synchronizedWriter struct {
	mu     sync.Mutex
	writer io.Writer
}

func (writer *synchronizedWriter) Write(data []byte) (int, error) {
	writer.mu.Lock()
	defer writer.mu.Unlock()
	return writer.writer.Write(data)
}

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

var unconfiguredPostgresSessionFactory = func() postgresSession {
	return newUnconfiguredPostgresSession()
}

var automaticRetentionTestHook func(context.Context)

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
	case "prune":
		return pruneCommand(args[1:], stdout, stderr)
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
	stderr = &synchronizedWriter{writer: stderr}
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
		if updateErr := recordNotStarted(auditStore, command, *outboundEnabled, exitCode, err,
			localrollback.Summary{
				Watched: watchedPaths, Uncaptured: failures, Complete: false,
				Error: err.Error(), Retained: false,
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
	auditSession, err := auditStore.Begin(command, time.Now())
	if err != nil {
		fmt.Fprintf(stderr, "unring: begin audit log: %v\n", err)
		return internalErrorExitCode
	}
	auditRecord := auditSession.Snapshot()
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
		_ = finishNotStarted(auditSession, *outboundEnabled, internalErrorExitCode, err,
			localrollback.Summary{
				Watched: watchPaths, Complete: false, Error: err.Error(),
				RetentionCap: capBytes, Retained: false,
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
		_ = finishNotStarted(auditSession, *outboundEnabled, internalErrorExitCode, err, fileSummary)
		fmt.Fprintf(stderr, "unring: record file snapshot: %v\n", err)
		return internalErrorExitCode
	}
	applyAutomaticRetention(context.Background(), auditStore, auditRecord.ID, capBytes, scope.RetentionDays, &fileSummary, stderr)
	if err := auditSession.Update(func(record *audit.Record) { record.Files = fileSummary }); err != nil {
		fmt.Fprintf(stderr, "unring: record start-time retention: %v\n", err)
		return internalErrorExitCode
	}
	printSnapshotStarted(stderr, fileSummary)

	var proxy postgresSession
	var httpsProxy *httpsproxy.Proxy
	var ghSession *ghshim.Session
	var finalized bool
	var auditError string
	var postChildSignals *postChildSignalHandler
	requestedDecision := "discard"
	completionKind := ""
	defer func() {
		if postChildSignals != nil {
			if received := postChildSignals.Stop(); received != nil {
				completionKind = completionKindAbnormalDiscard
				requestedDecision = "discard"
				exitCode = exitCodeForSignal(received)
			}
		}
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
			completionKind = completionKindAbnormalDiscard
			auditError = fmt.Sprintf("panic: %v", recovered)
			if exitCode == 0 {
				exitCode = internalErrorExitCode
			}
		}
		if outcome == "discarded" && completionKind == "" {
			completionKind = completionKindAbnormalDiscard
		}
		saveErr := auditSession.Update(func(record *audit.Record) {
			record.EndedAt = time.Now().UTC()
			record.ExitCode = exitCode
			record.Error = strings.TrimPrefix(auditError, "\n")
			record.Decision = requestedDecision
			record.CompletionKind = completionKind
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
		proxy = unconfiguredPostgresSessionFactory()
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
	postChildSignals = startPostChildSignalHandler(signalChannel, stderr)
	interrupted := result.Interrupted
	observePostChildSignal := func() {
		if received := postChildSignals.First(); received != nil {
			interrupted = true
			result.ExitCode = exitCodeForSignal(received)
			completionKind = completionKindAbnormalDiscard
			requestedDecision = "discard"
		}
	}
	postChildPhaseParent := func() context.Context {
		if interrupted {
			return context.Background()
		}
		return postChildSignals.Context()
	}
	if result.Err != nil {
		auditError = joinErrorText(auditError, result.Err)
	}
	printScanFinishing(stderr, fileSummary)
	startEvicted := append([]string(nil), fileSummary.Evicted...)
	startRetentionEvents := append([]localrollback.RetentionEvent(nil), fileSummary.RetentionEvents...)
	postChildSignals.SetPhase("post-session scan")
	fileSummary = sealFileSession(postChildSignals.Context(), fileSession, stderr)
	observePostChildSignal()
	fileSummary.Evicted = append(startEvicted, fileSummary.Evicted...)
	fileSummary.RetentionEvents = append(startRetentionEvents, fileSummary.RetentionEvents...)
	printScanFinished(stderr, fileSummary)
	postChildSignals.SetPhase("automatic retention")
	applyAutomaticRetention(postChildSignals.Context(), auditStore, auditRecord.ID, capBytes, scope.RetentionDays, &fileSummary, stderr)
	observePostChildSignal()
	if !fileSummary.Complete {
		if fileSummary.Interrupted {
			fmt.Fprintf(stderr, "unring: FILE CHANGE LIST INCOMPLETE: the post-session scan was interrupted; the recorded list may omit file changes. Run unring log --json %s for precise diagnostic details.\n", auditRecord.ID)
		} else if hasActionableFileCoverageFailure(fileSummary) {
			fmt.Fprintf(stderr, "unring: FILE COVERAGE INCOMPLETE: %s\n", fileSummary.Error)
		} else {
			fmt.Fprintln(stderr, "unring: FILE COVERAGE NOTE: unsupported file types remain recorded but cannot be restored per path.")
		}
	}
	printFileChanges(stdout, auditRecord.ID, fileSummary, !databaseConfigured)
	if err := auditSession.Update(func(record *audit.Record) { record.Files = fileSummary }); err != nil {
		auditError = joinErrorText(auditError, err)
		fmt.Fprintf(stderr, "unring: record file changes: %v\n", err)
	}

	ghSummary := ghshim.Summary{Sealed: true}
	httpsSummary := httpsproxy.Summary{Sealed: true, Finalized: true}
	var ghSealErr error
	var httpsSealErr error
	if *outboundEnabled {
		postChildSignals.SetPhase("gh sealing")
		ghSealContext, ghSealCancel := context.WithTimeout(postChildPhaseParent(), 10*time.Second)
		ghSealErr = ghSession.Seal(ghSealContext)
		ghSealCancel()
		observePostChildSignal()
		if interrupted && errors.Is(ghSealErr, context.Canceled) {
			ghSealContext, ghSealCancel = context.WithTimeout(context.Background(), 10*time.Second)
			ghSealErr = ghSession.Seal(ghSealContext)
			ghSealCancel()
		}
		ghSummary = ghSession.Summary()

		postChildSignals.SetPhase("HTTPS sealing")
		httpsSealContext, httpsSealCancel := context.WithTimeout(postChildPhaseParent(), 10*time.Second)
		httpsSealErr = httpsProxy.Seal(httpsSealContext)
		httpsSealCancel()
		observePostChildSignal()
		if interrupted && errors.Is(httpsSealErr, context.Canceled) {
			httpsSealContext, httpsSealCancel = context.WithTimeout(context.Background(), 10*time.Second)
			httpsSealErr = httpsProxy.Seal(httpsSealContext)
			httpsSealCancel()
		}
		httpsSummary = httpsProxy.Summary()
	}

	postChildSignals.SetPhase("Postgres sealing")
	sealContext, sealCancel := context.WithTimeout(postChildPhaseParent(), 10*time.Second)
	sealErr := proxy.Seal(sealContext)
	sealCancel()
	observePostChildSignal()
	if interrupted && errors.Is(sealErr, context.Canceled) {
		sealContext, sealCancel = context.WithTimeout(context.Background(), 10*time.Second)
		sealErr = proxy.Seal(sealContext)
		sealCancel()
	}
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
		completionKind = completionKindNoDecision
		if *forceDiscard {
			completionKind = completionKindExplicitDiscard
		}
		if interrupted || result.Err != nil || result.ExitCode != 0 || !fileSummary.Complete {
			completionKind = completionKindAbnormalDiscard
		}
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
			disclosure := quietSessionDisclosure
			if !fileSummary.Complete || len(fileSummary.Changes) != 0 {
				disclosure = quietSessionActivityDisclosure
			}
			fmt.Fprintln(stderr, disclosure)
		}
		postChildSignals.SetPhase("discard finalization")
		finalizeContext, finalizeCancel := context.WithTimeout(postChildPhaseParent(), 10*time.Second)
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
		observePostChildSignal()
		if interrupted && errors.Is(finalizeErr, context.Canceled) {
			postChildSignals.SetPhase("safe discard finalization")
			finalizeContext, finalizeCancel = context.WithTimeout(context.Background(), 10*time.Second)
			ghFinalizeErr = nil
			httpsFinalizeErr = nil
			if *outboundEnabled {
				ghFinalizeErr = ghSession.Finalize(finalizeContext, false)
				httpsFinalizeErr = httpsProxy.Finalize(finalizeContext, false)
			}
			finalizeErr = errors.Join(
				ghFinalizeErr, httpsFinalizeErr,
				proxy.Finalize(finalizeContext, pgproxy.DecisionRollback),
			)
			finalizeCancel()
		}
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
	postChildSignals.SetPhase("the final decision")
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
				stdin, stdout, postChildSignals.Context(), summary, httpsSummary, ghSummary,
			)
			if reviewErr != nil {
				fmt.Fprintf(stderr, "unring: %v; defaulting to discard\n", reviewErr)
				decision = pgproxy.DecisionRollback
			}
		} else {
			var promptInterrupted bool
			decision, promptInterrupted = promptDecisionWithSignal(stdin, stdout, postChildSignals.Context().Done())
			interrupted = interrupted || promptInterrupted
		}
	}

	observePostChildSignal()
	if interrupted {
		decision = pgproxy.DecisionRollback
	}
	if decision == pgproxy.DecisionRollback {
		completionKind = completionKindExplicitDiscard
		if interrupted || result.Err != nil || result.ExitCode != 0 || interceptionErr != nil || !fileSummary.Complete {
			completionKind = completionKindAbnormalDiscard
		}
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
		postChildSignals.SetPhase("gh finalization")
		ghFinalizeContext, ghFinalizeCancel := context.WithTimeout(postChildPhaseParent(), 30*time.Second)
		ghFinalizeErr = ghSession.Finalize(ghFinalizeContext, commitExternal)
		ghFinalizeCancel()
		observePostChildSignal()
		if interrupted && errors.Is(ghFinalizeErr, context.Canceled) {
			ghFinalizeContext, ghFinalizeCancel = context.WithTimeout(context.Background(), 30*time.Second)
			ghFinalizeErr = ghSession.Finalize(ghFinalizeContext, false)
			ghFinalizeCancel()
		}
	}
	if ghFinalizeErr != nil {
		auditError = joinErrorText(auditError, ghFinalizeErr)
	}
	commitHTTPS := commitExternal && ghFinalizeErr == nil
	var httpsFinalizeErr error
	if *outboundEnabled {
		postChildSignals.SetPhase("HTTPS finalization")
		httpsFinalizeContext, httpsFinalizeCancel := context.WithTimeout(postChildPhaseParent(), 30*time.Second)
		httpsFinalizeErr = httpsProxy.Finalize(httpsFinalizeContext, commitHTTPS)
		httpsFinalizeCancel()
		observePostChildSignal()
		if interrupted && errors.Is(httpsFinalizeErr, context.Canceled) {
			httpsFinalizeContext, httpsFinalizeCancel = context.WithTimeout(context.Background(), 30*time.Second)
			httpsFinalizeErr = httpsProxy.Finalize(httpsFinalizeContext, false)
			httpsFinalizeCancel()
		}
	}
	postgresDecision := decision
	if httpsFinalizeErr != nil || ghFinalizeErr != nil {
		// A staged HTTP replay may have partially succeeded. Keep the database
		// reversible instead of compounding that uncertainty with a commit.
		postgresDecision = pgproxy.DecisionRollback
		auditError = joinErrorText(auditError, httpsFinalizeErr)
	}
	postChildSignals.SetPhase("Postgres finalization")
	finalizeContext, finalizeCancel := context.WithTimeout(postChildPhaseParent(), 10*time.Second)
	postgresFinalizeErr := proxy.Finalize(finalizeContext, postgresDecision)
	finalizeCancel()
	observePostChildSignal()
	if interrupted && errors.Is(postgresFinalizeErr, context.Canceled) {
		finalizeContext, finalizeCancel = context.WithTimeout(context.Background(), 10*time.Second)
		postgresFinalizeErr = proxy.Finalize(finalizeContext, pgproxy.DecisionRollback)
		finalizeCancel()
	}
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

func recordNotStarted(
	store *audit.Store,
	command []string,
	outbound bool,
	exitCode int,
	reason error,
	files localrollback.Summary,
) error {
	session, err := store.Begin(command, time.Now())
	if err != nil {
		return err
	}
	return finishNotStarted(session, outbound, exitCode, reason, files)
}

func finishNotStarted(
	session *audit.Session,
	outbound bool,
	exitCode int,
	reason error,
	files localrollback.Summary,
) error {
	return session.Update(func(record *audit.Record) {
		record.Files = files
		record.EndedAt = time.Now().UTC()
		record.ExitCode = exitCode
		record.Error = reason.Error()
		record.Outcome = "not_started"
		record.Outbound = outbound
	})
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
		fmt.Fprintf(output, "unring: PATH OUTSIDE WHOLE-VOLUME BACKSTOP: %s: %s\n", humanPath(failure.Path), failure.Error)
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
		fmt.Fprintf(output, "unring: CHANGE-LIST SCAN INCOMPLETE: %s: %s\n", humanPath(failure.Path), failure.Error)
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

type postChildSignalHandler struct {
	ctx    context.Context
	cancel context.CancelFunc
	input  <-chan os.Signal
	output io.Writer
	stop   chan struct{}
	done   chan struct{}
	once   sync.Once

	mu          sync.Mutex
	phase       string
	first       os.Signal
	secondNamed bool
}

func startPostChildSignalHandler(input <-chan os.Signal, output io.Writer) *postChildSignalHandler {
	ctx, cancel := context.WithCancel(context.Background())
	handler := &postChildSignalHandler{
		ctx: ctx, cancel: cancel, input: input, output: output,
		stop: make(chan struct{}), done: make(chan struct{}), phase: "post-session scan",
	}
	go handler.run()
	return handler
}

func (handler *postChildSignalHandler) run() {
	defer close(handler.done)
	for {
		select {
		case received := <-handler.input:
			handler.record(received)
		case <-handler.stop:
			for {
				select {
				case received := <-handler.input:
					handler.record(received)
				default:
					return
				}
			}
		}
	}
}

func (handler *postChildSignalHandler) record(received os.Signal) {
	handler.mu.Lock()
	if handler.first == nil {
		handler.first = received
		phase := handler.phase
		handler.cancel()
		handler.mu.Unlock()
		if phase == "post-session scan" {
			fmt.Fprintf(handler.output, "unring: %s received during the post-session scan; stopping the scan and discarding the session.\n", postChildSignalName(received))
			return
		}
		fmt.Fprintf(handler.output, "unring: %s received during %s; stopping that phase and discarding the session.\n", postChildSignalName(received), phase)
		return
	}
	if !handler.secondNamed {
		handler.secondNamed = true
		handler.mu.Unlock()
		fmt.Fprintf(handler.output, "unring: second signal received (%s); safe discard finalization is already in progress and will not be skipped.\n", postChildSignalName(received))
		return
	}
	handler.mu.Unlock()
}

func (handler *postChildSignalHandler) Context() context.Context {
	return handler.ctx
}

func (handler *postChildSignalHandler) SetPhase(phase string) {
	handler.mu.Lock()
	handler.phase = phase
	handler.mu.Unlock()
}

func (handler *postChildSignalHandler) First() os.Signal {
	handler.mu.Lock()
	defer handler.mu.Unlock()
	return handler.first
}

func (handler *postChildSignalHandler) Stop() os.Signal {
	handler.once.Do(func() { close(handler.stop) })
	<-handler.done
	handler.cancel()
	return handler.First()
}

func sealFileSession(
	ctx context.Context,
	session *localrollback.Session,
	output io.Writer,
) localrollback.Summary {
	type sealResult struct {
		summary localrollback.Summary
	}
	results := make(chan sealResult, 1)
	progress := make(chan localrollback.SealProgress, 1)
	go func() {
		summary := session.SealContext(ctx, time.Now(), func(update localrollback.SealProgress) {
			select {
			case progress <- update:
			default:
			}
		})
		results <- sealResult{summary: summary}
	}()
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	started := time.Now()
	var latest localrollback.SealProgress
	nextProgress := 0
	for {
		select {
		case result := <-results:
			return result.summary
		case update := <-progress:
			latest = update
			if update.Total == 0 {
				continue
			}
			step := update.Total / 10
			if step < timeMachineProgressMinimumStep {
				step = timeMachineProgressMinimumStep
			}
			if update.Completed == 0 || update.Completed == update.Total || update.Completed >= nextProgress {
				fmt.Fprintf(output,
					"unring: Time Machine inclusion progress: %d of %d changed paths checked in batches.\n",
					update.Completed, update.Total)
				nextProgress = update.Completed + step
			}
		case <-ticker.C:
			if latest.Total > 0 {
				fmt.Fprintf(output,
					"unring: Time Machine inclusion is still running: %d of %d changed paths checked (%s elapsed).\n",
					latest.Completed, latest.Total, time.Since(started).Round(time.Second))
			} else {
				fmt.Fprintf(output, "unring: post-session filesystem scan is still running (%s elapsed).\n",
					time.Since(started).Round(time.Second))
			}
		}
	}
}

func postChildSignalName(signal os.Signal) string {
	switch signal {
	case os.Interrupt:
		return "interrupt"
	case syscall.SIGTERM:
		return "termination signal"
	default:
		return signal.String()
	}
}

const timeMachineProgressMinimumStep = 256

func exitCodeForSignal(signal os.Signal) int {
	if unixSignal, ok := signal.(syscall.Signal); ok {
		return 128 + int(unixSignal)
	}
	return internalErrorExitCode
}

func printFileChanges(output io.Writer, sessionID string, summary localrollback.Summary, announceCompleteZero bool) {
	if summary.Interrupted {
		fmt.Fprintf(output, "File change list incomplete: the post-session scan was interrupted, so this session must not be treated as having no file changes. Run unring log --json %s for precise diagnostic details.\n", sessionID)
		if len(summary.Changes) == 0 {
			return
		}
	}
	if len(summary.Changes) == 0 {
		if summary.Complete {
			if announceCompleteZero {
				fmt.Fprintln(output, "Files changed: 0 created, 0 modified, 0 deleted. The post-session scan completed.")
			}
		} else {
			fmt.Fprintln(output, "File change list incomplete: the post-session scan did not produce a complete result, so this session must not be treated as having no file changes.")
		}
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
	agentStateRoots, inferredAgentStateRoots := agentStateGroupingRoots(summary.AgentStateRoots)
	if inferredAgentStateRoots {
		printAgentStateGroupingInference(output, "", agentStateRoots)
	}
	regular, agentState := groupAgentStateChanges(summary.Changes, agentStateRoots)
	printChange := func(output io.Writer, change localrollback.Change) {
		printLiveChange(output, change, summary.Interrupted)
	}
	printWatchedRootChangeGroups(output, regular, summary.Watched, summary.ChangeListRoots, sessionID, printChange)
	if len(agentState) > 0 {
		fmt.Fprintln(output, "AGENT OWN-STATE CHANGES — reported, but skipped by restore --all unless explicitly included")
		printDeclaredRootChangeGroups(output, agentState, agentStateRoots, sessionID, "agent own-state changes", "AGENT-STATE ROOT CHANGES", printChange)
	}
	if summary.Retained && hasRestorableFileChange(summary.Changes) {
		fmt.Fprintf(output, "Restore later with: unring restore %s\n", sessionID)
	} else if summary.Retained {
		fmt.Fprintln(output, "No changed path has restorable snapshot data.")
	} else {
		fmt.Fprintln(output, "Snapshot data is no longer retained; these paths cannot be restored from this session.")
	}
}

func printLiveChange(output io.Writer, change localrollback.Change, interrupted bool) {
	fmt.Fprintf(output, "  %-8s %s\n", change.Kind, humanPath(change.Path))
	if change.RestoreSource == localrollback.RestoreSourceVolume {
		fmt.Fprintln(output, "             SNAPSHOT ONLY: restoring this path requires sudo to mount the APFS snapshot.")
	} else if change.UnrestorableReason != "" {
		fmt.Fprintf(output, "             NOT RESTORABLE: %s\n", humanUnrestorableReason(change.UnrestorableReason, interrupted))
	}
}

func printAuditFiles(output io.Writer, sessionID string, summary localrollback.Summary) {
	if len(summary.Watched) == 0 {
		return
	}
	fmt.Fprintln(output, "\nFILES — RESTORE COVERAGE")
	if len(summary.Changes) == 0 {
		switch {
		case summary.Interrupted:
			fmt.Fprintln(output, "  File change list incomplete: the post-session scan was interrupted; zero changes must not be inferred.")
		case summary.Complete:
			fmt.Fprintln(output, "  Files changed: 0 created, 0 modified, 0 deleted. The post-session scan completed.")
		default:
			fmt.Fprintln(output, "  File change list incomplete: the post-session scan did not produce a complete result; zero changes must not be inferred.")
		}
	}
	storageLabel := "measured storage bytes"
	if !summary.StorageExact {
		storageLabel = "storage-byte upper bound"
	}
	fmt.Fprintf(output, "  Snapshot: %s; %d %s (%d logical); retained: %t\n",
		summary.Storage, summary.StorageBytes, storageLabel, summary.LogicalBytes, summary.Retained)
	for _, failure := range summary.Uncaptured {
		if summary.Interrupted && isInternalCancellationFailure(failure.Error) {
			continue
		}
		printCaptureFailure(output, "  ", failure)
	}
	if summary.Backstop.Available {
		for _, snapshot := range summary.Backstop.Snapshots {
			fmt.Fprintf(output, "  Volume backstop: %s on %s\n", snapshot.Name, snapshot.MountPoint)
		}
	} else if summary.Backstop.Reason != "" {
		fmt.Fprintf(output, "  NO WHOLE-VOLUME BACKSTOP: %s\n", summary.Backstop.Reason)
	} else if !summary.Backstop.Checked {
		fmt.Fprintln(output, "  Whole-volume backstop was not checked for this session.")
	} else {
		fmt.Fprintln(output, "  NO WHOLE-VOLUME BACKSTOP: no reason was recorded.")
	}
	for _, failure := range summary.Backstop.Excluded {
		fmt.Fprintf(output, "  OUTSIDE VOLUME BACKSTOP: %s: %s\n", humanPath(failure.Path), failure.Error)
	}
	printStoredChangeListLimitation(output, summary, "  ", false)
	for _, failure := range summary.ScanFailures {
		if !summary.Interrupted {
			fmt.Fprintf(output, "  CHANGE-LIST SCAN INCOMPLETE: %s: %s\n", humanPath(failure.Path), failure.Error)
		}
	}
	if summary.ScanRoot != "" {
		fmt.Fprintf(output, "  Change-list scan: %d entries/%d ms before; %d entries/%d ms after\n",
			summary.ScanBeforeFiles, summary.ScanBeforeMillis,
			summary.ScanAfterFiles, summary.ScanAfterMillis)
	}
	for _, event := range summary.RetentionEvents {
		removal := localrollback.RetentionRemoval{
			SessionID: event.SessionID, StorageBytes: event.StorageBytes,
			StorageExact: event.StorageExact, Expired: event.Expired,
			CapRequired: event.CapRequired,
		}
		fmt.Fprintf(output, "  Retention removed %s: %s; %s.\n",
			event.SessionID, retentionReason(removal), retentionSpace(removal))
	}
	agentStateRoots, inferredAgentStateRoots := agentStateGroupingRoots(summary.AgentStateRoots)
	if inferredAgentStateRoots {
		printAgentStateGroupingInference(output, "  ", agentStateRoots)
	}
	regular, agentState := groupAgentStateChanges(summary.Changes, agentStateRoots)
	printChange := func(output io.Writer, change localrollback.Change) {
		printStoredChange(output, change, summary.Interrupted)
	}
	printWatchedRootChangeGroups(output, regular, summary.Watched, summary.ChangeListRoots, sessionID, printChange)
	if len(agentState) > 0 {
		fmt.Fprintln(output, "  AGENT OWN-STATE CHANGES — reported, but skipped by restore --all unless explicitly included")
		printDeclaredRootChangeGroups(output, agentState, agentStateRoots, sessionID, "agent own-state changes", "  AGENT-STATE ROOT CHANGES", printChange)
	}
	if summary.Interrupted {
		fmt.Fprintf(output, "  INCOMPLETE: the post-session scan was interrupted; the recorded change list may omit file changes. Run unring log --json %s for precise diagnostic details.\n", sessionID)
	} else if summary.Error != "" {
		if hasActionableFileCoverageFailure(summary) {
			fmt.Fprintf(output, "  INCOMPLETE: %s\n", summary.Error)
		} else {
			fmt.Fprintf(output, "  Coverage note: %s\n", summary.Error)
		}
	}
}

func printCaptureFailure(output io.Writer, prefix string, failure localrollback.CaptureFailure) {
	if localrollback.IsUnsupportedFileTypeFailure(failure) {
		fmt.Fprintf(output, "%sUNSUPPORTED FILE TYPE (informational): %s: %s\n", prefix, humanPath(failure.Path), failure.Error)
		return
	}
	fmt.Fprintf(output, "%sFILE NOT SNAPSHOTTED: %s: %s\n", prefix, humanPath(failure.Path), failure.Error)
}

func hasActionableFileCoverageFailure(summary localrollback.Summary) bool {
	return !localrollback.HasOnlyUnsupportedFileTypeFailures(summary)
}

func printStoredChange(output io.Writer, change localrollback.Change, interrupted bool) {
	fmt.Fprintf(output, "  %-8s %s\n", change.Kind, humanPath(change.Path))
	if change.RestoreSource == localrollback.RestoreSourceVolume {
		fmt.Fprintln(output, "             SNAPSHOT ONLY: requires sudo to mount the APFS snapshot")
	} else if change.UnrestorableReason != "" {
		fmt.Fprintf(output, "             NOT RESTORABLE: %s\n", humanUnrestorableReason(change.UnrestorableReason, interrupted))
	}
}

func humanUnrestorableReason(reason string, interrupted bool) string {
	if interrupted && isInternalCancellationFailure(reason) {
		return "the post-session scan was interrupted before this path's restore coverage could be verified"
	}
	return reason
}

func isInternalCancellationFailure(reason string) bool {
	return strings.Contains(reason, context.Canceled.Error()) || strings.Contains(reason, context.DeadlineExceeded.Error())
}

func printBoundedChanges(
	output io.Writer,
	changes []localrollback.Change,
	sessionID string,
	group string,
	printChange func(io.Writer, localrollback.Change),
) {
	shown, truncated := boundedHumanSessionCount(len(changes), false)
	for _, change := range changes[:shown] {
		printChange(output, change)
	}
	if truncated {
		withheld := len(changes) - shown
		fmt.Fprintf(output,
			"Showing %d of %d %s; %d withheld. Run unring restore %s to show every recorded change.\n",
			shown, len(changes), group, withheld, sessionID)
	}
}

type watchedRootChangeGroup struct {
	root    string
	changes []localrollback.Change
}

func printWatchedRootChangeGroups(
	output io.Writer,
	changes []localrollback.Change,
	watched []string,
	changeListRoots []string,
	sessionID string,
	printChange func(io.Writer, localrollback.Change),
) {
	if len(changes) == 0 {
		return
	}
	groups := make([]watchedRootChangeGroup, 0, len(watched)+1)
	rootIndexes := make(map[string]int)
	for _, root := range watched {
		root = filepath.Clean(root)
		if _, exists := rootIndexes[root]; exists {
			continue
		}
		rootIndexes[root] = len(groups)
		groups = append(groups, watchedRootChangeGroup{root: root})
	}
	outsideIndexes := make(map[string]int)
	for _, change := range changes {
		matchedRoot := ""
		matchedIndex := -1
		for root, index := range rootIndexes {
			if pathWithinRoot(change.Path, root) && len(root) > len(matchedRoot) {
				matchedRoot = root
				matchedIndex = index
			}
		}
		if matchedIndex < 0 {
			outsideRoot := outsidePresentationRoot(change.Path, changeListRoots)
			var exists bool
			matchedIndex, exists = outsideIndexes[outsideRoot]
			if !exists {
				matchedIndex = len(groups)
				outsideIndexes[outsideRoot] = matchedIndex
				groups = append(groups, watchedRootChangeGroup{root: outsideRoot})
			}
		}
		groups[matchedIndex].changes = append(groups[matchedIndex].changes, change)
	}
	nonempty := 0
	for _, group := range groups {
		if len(group.changes) > 0 {
			nonempty++
		}
	}
	showHeadings := len(watched) > 1 || len(outsideIndexes) > 0 || nonempty > 1
	for _, group := range groups {
		if len(group.changes) == 0 {
			continue
		}
		_, outside := outsideIndexes[group.root]
		label := "changes outside watched roots"
		if !outside && group.root != "" {
			label = "changes under watched root " + humanPath(group.root)
		} else if outside && len(outsideIndexes) > 1 {
			label = "changes outside watched roots under " + humanPath(group.root)
		}
		if showHeadings {
			if outside {
				fmt.Fprintf(output, "CHANGES OUTSIDE WATCHED ROOTS — %s\n", humanPath(group.root))
			} else {
				fmt.Fprintf(output, "WATCHED ROOT CHANGES — %s\n", humanPath(group.root))
			}
		}
		printBoundedChanges(output, group.changes, sessionID, label, printChange)
	}
}

func outsidePresentationRoot(path string, changeListRoots []string) string {
	path = filepath.Clean(path)
	base := ""
	for _, candidate := range changeListRoots {
		candidate = filepath.Clean(candidate)
		if pathWithinRoot(path, candidate) && len(candidate) > len(base) {
			base = candidate
		}
	}
	if base == "" {
		base = filepath.VolumeName(path) + string(os.PathSeparator)
	}
	relative, err := filepath.Rel(base, path)
	if err != nil || relative == "." {
		return base
	}
	first := strings.Split(relative, string(os.PathSeparator))[0]
	return filepath.Join(base, first)
}

func printDeclaredRootChangeGroups(
	output io.Writer,
	changes []localrollback.Change,
	roots []string,
	sessionID string,
	groupLabel string,
	heading string,
	printChange func(io.Writer, localrollback.Change),
) {
	groups := make([]watchedRootChangeGroup, 0, len(roots)+1)
	rootIndexes := make(map[string]int)
	for _, root := range roots {
		root = filepath.Clean(root)
		if _, exists := rootIndexes[root]; exists {
			continue
		}
		rootIndexes[root] = len(groups)
		groups = append(groups, watchedRootChangeGroup{root: root})
	}
	unmatched := -1
	for _, change := range changes {
		matchedRoot := ""
		matchedIndex := -1
		for root, index := range rootIndexes {
			if pathWithinRoot(change.Path, root) && len(root) > len(matchedRoot) {
				matchedRoot = root
				matchedIndex = index
			}
		}
		if matchedIndex < 0 {
			if unmatched < 0 {
				unmatched = len(groups)
				groups = append(groups, watchedRootChangeGroup{})
			}
			matchedIndex = unmatched
		}
		groups[matchedIndex].changes = append(groups[matchedIndex].changes, change)
	}
	nonempty := 0
	for _, group := range groups {
		if len(group.changes) > 0 {
			nonempty++
		}
	}
	for _, group := range groups {
		if len(group.changes) == 0 {
			continue
		}
		label := groupLabel
		if nonempty > 1 && group.root != "" {
			fmt.Fprintf(output, "%s — %s\n", heading, humanPath(group.root))
			label += " under " + humanPath(group.root)
		}
		printBoundedChanges(output, group.changes, sessionID, label, printChange)
	}
}

func pathWithinRoot(path, root string) bool {
	path = filepath.Clean(path)
	return path == root || strings.HasPrefix(path, root+string(os.PathSeparator))
}

func groupAgentStateChanges(changes []localrollback.Change, roots []string) ([]localrollback.Change, []localrollback.Change) {
	regular := make([]localrollback.Change, 0, len(changes))
	agentState := make([]localrollback.Change, 0)
	for _, change := range changes {
		if isAgentStateChange(change, roots) {
			agentState = append(agentState, change)
		} else {
			regular = append(regular, change)
		}
	}
	return regular, agentState
}

func agentStateGroupingRoots(roots []string) ([]string, bool) {
	if len(roots) > 0 {
		return roots, false
	}
	return localrollback.AgentStateRoots(""), true
}

func printAgentStateGroupingInference(output io.Writer, prefix string, roots []string) {
	if len(roots) == 0 {
		fmt.Fprintf(output, "%sAGENT-STATE GROUPING INFERRED: this record has no recorded agent-state roots, and none could be determined from the current environment; no path is classified as agent own-state.\n", prefix)
		return
	}
	fmt.Fprintf(output, "%sAGENT-STATE GROUPING INFERRED: this record has no recorded agent-state roots; grouping uses the current environment: %s\n",
		prefix, strings.Join(roots, ", "))
}

func isAgentStateChange(change localrollback.Change, roots []string) bool {
	return localrollback.IsAgentStatePathWithin(change.Path, roots)
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
	showAll := flags.Bool("all", false, "show every recorded session in human-readable output (JSON is always complete)")
	flags.Usage = func() {
		fmt.Fprintln(stderr, "Usage: unring log [--json] [--all] [session-id]")
	}
	if err := flags.Parse(args); err != nil {
		return usageExitCode
	}
	if flags.NArg() > 1 {
		flags.Usage()
		return usageExitCode
	}
	if flags.NArg() == 1 && *showAll {
		fmt.Fprintln(stderr, "unring: --all cannot be combined with a session id")
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
		total := len(records)
		shown, truncated := boundedHumanSessionCount(total, *showAll || *asJSON)
		if truncated {
			records = records[:shown]
		}
		if *asJSON {
			return writeJSON(stdout, stderr, records)
		}
		if len(records) == 0 {
			fmt.Fprintln(stdout, "No unring sessions have been recorded.")
			return 0
		}
		fmt.Fprintln(stdout, "SESSION ID                                  STARTED               OUTCOME          COMMAND")
		for _, record := range records {
			fmt.Fprintf(stdout, "%-43s %-21s %-16s %s\n",
				record.ID, record.StartedAt.Local().Format("2006-01-02 15:04:05"),
				displayedOutcome(record), humanCommand(record.Command))
		}
		if truncated {
			fmt.Fprintf(stdout, "Showing the newest %d of %d sessions; use unring log --all to show everything.\n", defaultSessionListLimit, total)
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

func pruneCommand(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("unring prune", flag.ContinueOnError)
	flags.SetOutput(stderr)
	confirm := flags.String("confirm", "", "remove the exact set identified by a prune preview token")
	showAll := flags.Bool("all", false, "show every session in the retention set")
	flags.Usage = func() {
		fmt.Fprintln(stderr, "Usage: unring prune [--all] [--confirm preview-token]")
		fmt.Fprintln(stderr, "Without --confirm, show the sessions the age and byte limits would remove.")
	}
	if err := flags.Parse(args); err != nil {
		return usageExitCode
	}
	if flags.NArg() != 0 {
		flags.Usage()
		return usageExitCode
	}
	if *confirm != "" && *showAll {
		fmt.Fprintln(stderr, "unring: --all cannot be combined with --confirm")
		return usageExitCode
	}
	store, err := audit.OpenStore()
	if err != nil {
		fmt.Fprintf(stderr, "unring: open state store: %v\n", err)
		return internalErrorExitCode
	}
	if *confirm != "" {
		now := time.Now()
		preview, err := loadPrunePreview(store.StateDir(), *confirm, now)
		if err != nil {
			_ = cleanupPrunePreviews(store.StateDir(), now, "")
			fmt.Fprintf(stderr, "unring: load prune preview %q: %v\n", *confirm, err)
			return usageExitCode
		}
		if err := cleanupPrunePreviews(store.StateDir(), now, *confirm); err != nil {
			fmt.Fprintf(stderr, "unring: clean expired prune previews: %v\n", err)
			return internalErrorExitCode
		}
		if _, err := applyRetentionRemovals(context.Background(), store, preview.Removals, func() error {
			return validatePrunePreview(store, preview, time.Now())
		}, nil, stdout, "removed"); err != nil {
			printPartialRetentionFailure(stderr, err)
			fmt.Fprintf(stderr, "unring: prune preview %q was not fully applied: %v; run unring prune again if no removals were reported above.\n", *confirm, err)
			return internalErrorExitCode
		}
		if err := os.Remove(prunePreviewPath(store.StateDir(), *confirm)); err != nil && !errors.Is(err, os.ErrNotExist) {
			fmt.Fprintf(stderr, "unring: remove completed prune preview: %v\n", err)
			return internalErrorExitCode
		}
		fmt.Fprintln(stdout, "Reported bytes are unring's retained-snapshot accounting, not a promise of immediately increased free disk space; copy-on-write clones may only release references to shared blocks.")
		return 0
	}
	if err := cleanupPrunePreviews(store.StateDir(), time.Now(), ""); err != nil {
		fmt.Fprintf(stderr, "unring: clean expired prune previews: %v\n", err)
		return internalErrorExitCode
	}
	capBytes, err := localrollback.RetentionCapForState(store.StateDir())
	if err != nil {
		fmt.Fprintf(stderr, "unring: read snapshot retention cap: %v\n", err)
		return usageExitCode
	}
	retentionDays, err := localrollback.RetentionDaysForState(store.StateDir())
	if err != nil {
		fmt.Fprintf(stderr, "unring: read session retention age: %v\n", err)
		return usageExitCode
	}
	records, listErr := store.List()
	if listErr != nil {
		if records == nil {
			fmt.Fprintf(stderr, "unring: inspect stored sessions for retention: %v\n", listErr)
			return internalErrorExitCode
		}
		fmt.Fprintf(stderr, "unring: retention warning: unreadable audit records were skipped: %v\n", listErr)
	}
	plan, err := localrollback.PlanRetention(
		store.StateDir(), storedSessions(records), capBytes,
		time.Duration(retentionDays)*24*time.Hour, time.Now(),
	)
	if err != nil {
		fmt.Fprintf(stderr, "unring: plan retention: %v\n", err)
		return internalErrorExitCode
	}
	printRetentionWarnings(stderr, plan.Warnings)
	if len(plan.Removals) == 0 {
		fmt.Fprintln(stdout, "Nothing to prune; the newest session and all other stored sessions are within the configured limits.")
		return 0
	}
	shownCount, truncated := boundedHumanSessionCount(len(plan.Removals), *showAll)
	shown := plan.Removals
	if truncated {
		shown = shown[:shownCount]
	}
	for _, removal := range shown {
		printRetentionRemoval(stdout, "would remove", removal)
	}
	fmt.Fprintln(stdout, "Reported bytes are unring's retained-snapshot accounting, not a promise of immediately increased free disk space; copy-on-write clones may only release references to shared blocks.")
	if truncated {
		fmt.Fprintf(stdout, "Showing %d of %d sessions in the retention set; use unring prune --all to name every session before confirming.\n", defaultSessionListLimit, len(plan.Removals))
		fmt.Fprintln(stdout, "No sessions were removed, and no confirmation token was issued for this truncated listing.")
		return 0
	}
	token, err := savePrunePreview(store.StateDir(), plan.Removals)
	if err != nil {
		fmt.Fprintf(stderr, "unring: save prune preview: %v\n", err)
		return internalErrorExitCode
	}
	fmt.Fprintf(stdout, "No sessions were removed. Run unring prune --confirm %s to remove exactly the set shown above.\n", token)
	return 0
}

const (
	prunePreviewVersion  = 1
	prunePreviewLifetime = 24 * time.Hour
)

type prunePreview struct {
	Version  int                              `json:"version"`
	Created  time.Time                        `json:"created_at"`
	Removals []localrollback.RetentionRemoval `json:"removals"`
}

func savePrunePreview(stateDir string, removals []localrollback.RetentionRemoval) (string, error) {
	var random [12]byte
	if _, err := rand.Read(random[:]); err != nil {
		return "", err
	}
	token := hex.EncodeToString(random[:])
	directory := filepath.Join(stateDir, "prune-previews")
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return "", err
	}
	data, err := json.MarshalIndent(prunePreview{
		Version: prunePreviewVersion, Created: time.Now().UTC(),
		Removals: append([]localrollback.RetentionRemoval(nil), removals...),
	}, "", "  ")
	if err != nil {
		return "", err
	}
	data = append(data, '\n')
	temporary, err := os.CreateTemp(directory, ".prune-preview-*.tmp")
	if err != nil {
		return "", err
	}
	temporaryPath := temporary.Name()
	defer func() { _ = os.Remove(temporaryPath) }()
	if err := temporary.Chmod(0o600); err != nil {
		_ = temporary.Close()
		return "", err
	}
	if _, err := temporary.Write(data); err != nil {
		_ = temporary.Close()
		return "", err
	}
	if err := temporary.Close(); err != nil {
		return "", err
	}
	if err := os.Rename(temporaryPath, prunePreviewPath(stateDir, token)); err != nil {
		return "", err
	}
	return token, nil
}

func loadPrunePreview(stateDir, token string, now time.Time) (prunePreview, error) {
	if len(token) != 24 {
		return prunePreview{}, errors.New("preview token must contain 24 hexadecimal characters")
	}
	if _, err := hex.DecodeString(token); err != nil {
		return prunePreview{}, errors.New("preview token must contain 24 hexadecimal characters")
	}
	data, err := os.ReadFile(prunePreviewPath(stateDir, token))
	if err != nil {
		return prunePreview{}, err
	}
	var preview prunePreview
	if err := json.Unmarshal(data, &preview); err != nil {
		return prunePreview{}, err
	}
	if preview.Version != prunePreviewVersion || len(preview.Removals) == 0 {
		return prunePreview{}, errors.New("unsupported or empty prune preview")
	}
	if preview.Created.IsZero() || preview.Created.After(now.Add(5*time.Minute)) {
		return prunePreview{}, errors.New("prune preview has an invalid creation time")
	}
	if now.Sub(preview.Created) > prunePreviewLifetime {
		return prunePreview{}, errors.New("prune preview expired after 24 hours; run unring prune again")
	}
	return preview, nil
}

func prunePreviewPath(stateDir, token string) string {
	return filepath.Join(stateDir, "prune-previews", token+".json")
}

func cleanupPrunePreviews(stateDir string, now time.Time, keepToken string) error {
	directory := filepath.Join(stateDir, "prune-previews")
	entries, err := os.ReadDir(directory)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if entry.IsDir() || entry.Name() == keepToken+".json" {
			continue
		}
		path := filepath.Join(directory, entry.Name())
		remove := false
		if strings.HasSuffix(entry.Name(), ".json") {
			data, readErr := os.ReadFile(path)
			var preview prunePreview
			if readErr == nil && json.Unmarshal(data, &preview) == nil && !preview.Created.IsZero() {
				remove = now.Sub(preview.Created) > prunePreviewLifetime || preview.Created.After(now.Add(5*time.Minute))
			} else if info, infoErr := entry.Info(); infoErr == nil {
				remove = now.Sub(info.ModTime()) > prunePreviewLifetime
			}
		} else if strings.HasPrefix(entry.Name(), ".prune-preview-") {
			if info, infoErr := entry.Info(); infoErr == nil {
				remove = now.Sub(info.ModTime()) > prunePreviewLifetime
			}
		}
		if remove {
			if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
		}
	}
	return nil
}

func validatePrunePreview(store *audit.Store, preview prunePreview, now time.Time) error {
	if preview.Created.IsZero() || now.Sub(preview.Created) > prunePreviewLifetime || preview.Created.After(now.Add(5*time.Minute)) {
		return errors.New("prune preview is stale")
	}
	records, err := store.List()
	if err != nil {
		return fmt.Errorf("cannot verify the current newest session and retention set: %w", err)
	}
	newest := ""
	if len(records) > 0 {
		newest = records[0].ID
	}
	for _, removal := range preview.Removals {
		if removal.SessionID == newest {
			return fmt.Errorf("session %s is now the newest stored session", removal.SessionID)
		}
	}
	capBytes, err := localrollback.RetentionCapForState(store.StateDir())
	if err != nil {
		return fmt.Errorf("read current snapshot retention cap: %w", err)
	}
	retentionDays, err := localrollback.RetentionDaysForState(store.StateDir())
	if err != nil {
		return fmt.Errorf("read current session retention age: %w", err)
	}
	plan, err := localrollback.PlanRetentionWhileLocked(
		store.StateDir(), storedSessions(records), capBytes,
		time.Duration(retentionDays)*24*time.Hour, now,
	)
	if err != nil {
		return fmt.Errorf("recheck current retention set: %w", err)
	}
	current := make(map[string]localrollback.RetentionRemoval, len(plan.Removals))
	for _, removal := range plan.Removals {
		current[removal.SessionID] = removal
	}
	for _, saved := range preview.Removals {
		updated, ok := current[saved.SessionID]
		if !ok || updated.Expired != saved.Expired || updated.CapRequired != saved.CapRequired || updated.HasSnapshot != saved.HasSnapshot {
			return fmt.Errorf("session %s is no longer in the same retention set", saved.SessionID)
		}
	}
	return nil
}

func storedSessions(records []audit.Record) []localrollback.StoredSession {
	sessions := make([]localrollback.StoredSession, 0, len(records))
	for _, record := range records {
		sessions = append(sessions, localrollback.StoredSession{
			ID: record.ID, StartedAt: record.StartedAt,
			StorageBytes: record.Files.StorageBytes,
			StorageExact: record.Files.StorageExact,
			StorageKnown: record.Files.Retained,
		})
	}
	return sessions
}

func retentionReason(removal localrollback.RetentionRemoval) string {
	switch {
	case removal.Expired && removal.CapRequired:
		return "past the configured age and needed to meet the byte cap"
	case removal.Expired:
		return "past the configured age"
	default:
		return "needed to meet the byte cap"
	}
}

func retentionSpace(removal localrollback.RetentionRemoval) string {
	if removal.StorageExact {
		return fmt.Sprintf("releases %d measured retained-snapshot bytes/references, not necessarily %d bytes of immediate free disk space", removal.StorageBytes, removal.StorageBytes)
	}
	return fmt.Sprintf("releases snapshot references with a %d-byte upper-bound accounting value, not a free-disk-space estimate", removal.StorageBytes)
}

func printRetentionRemoval(output io.Writer, verb string, removal localrollback.RetentionRemoval) {
	if verb == "retention removed" && !removal.Expired {
		fmt.Fprintf(output, "unring: retention evicted oldest snapshot %s; the audit record remains available.\n", removal.SessionID)
		return
	}
	target := retentionTarget(removal)
	space := retentionSpace(removal)
	if !removal.HasSnapshot {
		space = "this removes only the audit record; no snapshot data remains"
	}
	fmt.Fprintf(output, "%s session %s (%s): %s; %s.\n",
		verb, removal.SessionID, target, retentionReason(removal), space)
	if !removal.Expired {
		fmt.Fprintln(output, "  The audit record remains available; only clone restore data is removed.")
	}
}

func retentionTarget(removal localrollback.RetentionRemoval) string {
	if removal.Expired && removal.HasSnapshot {
		return "stored session and clone snapshot"
	}
	if removal.Expired {
		return "stored session audit record"
	}
	return "clone snapshot only"
}

func printRetentionWarnings(output io.Writer, warnings []localrollback.RetentionWarning) {
	for _, warning := range warnings {
		fmt.Fprintf(output, "unring: retention warning for %s: %s.\n", warning.SessionID, warning.Error)
	}
}

func applyRetentionRemovals(
	ctx context.Context,
	store *audit.Store,
	removals []localrollback.RetentionRemoval,
	validate func() error,
	summary *localrollback.Summary,
	output io.Writer,
	verb string,
) ([]localrollback.RetentionRemoval, error) {
	var applied []localrollback.RetentionRemoval
	err := localrollback.ApplyRetentionRemovalsContext(
		ctx,
		store.StateDir(), removals, validate,
		func(removal localrollback.RetentionRemoval) error {
			if removal.Expired {
				return store.DeleteWhileSessionLocked(removal.SessionID)
			}
			record, err := store.LoadExact(removal.SessionID)
			if errors.Is(err, os.ErrNotExist) {
				return nil
			}
			if err != nil {
				return err
			}
			record.Files.Retained = false
			return store.SaveWhileSessionLocked(record)
		},
		func(removal localrollback.RetentionRemoval) {
			applied = append(applied, removal)
			if summary != nil {
				summary.Evicted = append(summary.Evicted, removal.SessionID)
				summary.RetentionEvents = append(summary.RetentionEvents, localrollback.RetentionEvent{
					SessionID: removal.SessionID, StorageBytes: removal.StorageBytes,
					StorageExact: removal.StorageExact, Expired: removal.Expired,
					CapRequired: removal.CapRequired,
				})
			}
			if output != nil {
				printRetentionRemoval(output, verb, removal)
			}
		},
	)
	return applied, err
}

func applyAutomaticRetention(
	ctx context.Context,
	store *audit.Store,
	activeSession string,
	capBytes int64,
	retentionDays int,
	summary *localrollback.Summary,
	output io.Writer,
) {
	if automaticRetentionTestHook != nil {
		automaticRetentionTestHook(ctx)
	}
	if err := ctx.Err(); err != nil {
		return
	}
	records, err := store.ListContext(ctx)
	if err != nil {
		if errors.Is(err, context.Canceled) {
			return
		}
		if records == nil {
			fmt.Fprintf(output, "unring: enforce session retention: cannot inspect stored sessions: %v\n", err)
			return
		}
		fmt.Fprintf(output, "unring: retention warning: unreadable audit records were skipped: %v\n", err)
	}
	plan, err := localrollback.PlanRetentionContext(
		ctx,
		store.StateDir(), storedSessions(records), capBytes,
		time.Duration(retentionDays)*24*time.Hour, time.Now(),
	)
	if err != nil {
		if errors.Is(err, context.Canceled) {
			return
		}
		fmt.Fprintf(output, "unring: enforce session retention: %v\n", err)
		return
	}
	printRetentionWarnings(output, plan.Warnings)
	removals := make([]localrollback.RetentionRemoval, 0, len(plan.Removals))
	for _, removal := range plan.Removals {
		if removal.SessionID != activeSession {
			removals = append(removals, removal)
		}
	}
	applied, err := applyRetentionRemovals(ctx, store, removals, nil, summary, nil, "retention removed")
	printAutomaticRetentionRemovals(output, activeSession, applied)
	if err != nil {
		if errors.Is(err, context.Canceled) {
			return
		}
		printPartialRetentionFailure(output, err)
		fmt.Fprintf(output, "unring: enforce session retention: %v\n", err)
		return
	}
	if plan.After.Exact && plan.After.Bytes > plan.After.CapBytes {
		fmt.Fprintf(output,
			"unring: newest snapshot remains retained; measured snapshot usage is %d bytes, above the %d-byte cap.\n",
			plan.After.Bytes, plan.After.CapBytes)
	}
}

func printAutomaticRetentionRemovals(output io.Writer, activeSession string, removals []localrollback.RetentionRemoval) {
	if len(removals) == 0 {
		return
	}
	noun := "sessions"
	if len(removals) == 1 {
		noun = "session"
	}
	fmt.Fprintf(output, "unring: automatic retention removed %d %s.\n", len(removals), noun)
	shown, truncated := boundedHumanSessionCount(len(removals), false)
	shownRemovals := removals[:shown]
	if len(shownRemovals) == 1 {
		printRetentionRemoval(output, "retention removed", shownRemovals[0])
	} else {
		printCompactRetentionRemovals(output, shownRemovals)
	}
	if truncated {
		fmt.Fprintf(output,
			"unring: showing %d of %d automatic retention removals; %d withheld. Run unring log %s to see every removal recorded for this session.\n",
			shown, len(removals), len(removals)-shown, activeSession)
	}
}

func printCompactRetentionRemovals(output io.Writer, removals []localrollback.RetentionRemoval) {
	type removalGroup struct {
		label string
		ids   []string
	}
	var groups []removalGroup
	groupIndexes := make(map[string]int)
	var measuredBytes, upperBoundBytes int64
	for _, removal := range removals {
		target := retentionTarget(removal)
		if !removal.Expired {
			target += "; audit record retained"
		}
		label := retentionReason(removal) + "; removed " + target
		index, exists := groupIndexes[label]
		if !exists {
			index = len(groups)
			groupIndexes[label] = index
			groups = append(groups, removalGroup{label: label})
		}
		groups[index].ids = append(groups[index].ids, removal.SessionID)
		if removal.StorageExact {
			measuredBytes += removal.StorageBytes
		} else {
			upperBoundBytes += removal.StorageBytes
		}
	}
	for _, group := range groups {
		fmt.Fprintf(output, "unring: %s (%d sessions):\n", group.label, len(group.ids))
		for start := 0; start < len(group.ids); start += automaticRetentionIDsPerLine {
			end := start + automaticRetentionIDsPerLine
			if end > len(group.ids) {
				end = len(group.ids)
			}
			fmt.Fprintf(output, "  %s\n", strings.Join(group.ids[start:end], ", "))
		}
	}
	fmt.Fprintf(output,
		"unring: shown-removal accounting: %d measured retained-snapshot bytes/references; %d bytes of upper-bound accounting.\n",
		measuredBytes, upperBoundBytes)
	fmt.Fprintln(output, "unring: deleting copy-on-write clone references does not promise the same increase in immediate free disk space.")
}

const automaticRetentionIDsPerLine = 5

func printPartialRetentionFailure(output io.Writer, err error) {
	var applyErr *localrollback.RetentionApplyError
	if !errors.As(err, &applyErr) || !applyErr.SnapshotRemoved {
		return
	}
	fmt.Fprintf(output,
		"unring: IMPORTANT: clone snapshot data for %s was removed before its audit record could be updated; the audit record may still claim restore data is retained.\n",
		applyErr.Removal.SessionID)
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
	unlockStoredSession, err := localrollback.AcquireSessionReadLock(store.StateDir(), record.ID)
	if err != nil {
		fmt.Fprintf(stderr, "unring: lock restore session %s: %v\n", record.ID, err)
		return internalErrorExitCode
	}
	defer unlockStoredSession()
	record, err = store.LoadExact(record.ID)
	if err != nil {
		fmt.Fprintf(stderr, "unring: session %s was pruned before restore began: %v\n", record.ID, err)
		return internalErrorExitCode
	}
	record.Files = loadStoredFileSummary(store.StateDir(), record)
	if len(record.Files.Changes) == 0 {
		if !record.Files.Complete {
			fmt.Fprintf(stdout, "UNRING FILE CHANGES %s\n", record.ID)
			printStoredChangeListLimitation(stdout, record.Files, "  ", true)
			if record.Files.Interrupted {
				fmt.Fprintf(stdout, "INCOMPLETE: the post-session scan was interrupted; no complete change list is available, and this session must not be treated as clean. Run unring log --json %s for precise diagnostic details.\n", record.ID)
			} else {
				fmt.Fprintln(stdout, "INCOMPLETE: the post-session scan did not produce a complete change list, and this session must not be treated as clean.")
			}
			return 0
		}
		fmt.Fprintf(stdout, "Session %s changed no watched files; the post-session scan completed.\n", record.ID)
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
		agentStateRoots, inferredAgentStateRoots := agentStateGroupingRoots(record.Files.AgentStateRoots)
		if inferredAgentStateRoots {
			printAgentStateGroupingInference(stdout, "", agentStateRoots)
		}
		for _, change := range record.Files.Changes {
			if !includeAgentState && isAgentStateChange(change, agentStateRoots) {
				skippedAgentState = append(skippedAgentState, change)
				continue
			}
			selections = append(selections, change.Path)
		}
		if len(skippedAgentState) > 0 {
			fmt.Fprintln(stdout, "Skipped agent own-state paths; restore --all excludes them by default:")
			for _, change := range skippedAgentState {
				fmt.Fprintf(stdout, "  skipped  %s\n", humanPath(change.Path))
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
		fmt.Fprintf(stderr, "unring: snapshot-only path: %s\n", humanPath(change.Path))
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
			fmt.Fprintf(stdout, "restored  %s\n", humanPath(result.Path))
		case "already-restored":
			fmt.Fprintf(stdout, "already restored  %s\n", humanPath(result.Path))
		case "refused":
			exitCode = internalErrorExitCode
			if result.Err != nil {
				restoreEvent.Error = result.Err.Error()
				fmt.Fprintf(stderr, "refused   %s: %v; not restored\n", humanPath(result.Path), result.Err)
				break
			}
			fmt.Fprintf(stderr, "refused   %s: changed after the session ended; not overwritten\n", humanPath(result.Path))
			if result.Sidecar != "" {
				fmt.Fprintf(stderr, "snapshot version written alongside: %s\n", humanPath(result.Sidecar))
			} else {
				fmt.Fprintln(stderr, "pre-session state was absence, so there is no snapshot file to write alongside")
			}
			fmt.Fprintln(stderr, "rerun with --force to overwrite the conflict explicitly")
		case "unavailable":
			exitCode = internalErrorExitCode
			if result.Err != nil {
				restoreEvent.Error = result.Err.Error()
				fmt.Fprintf(stderr, "unavailable %s: %v\n", humanPath(result.Path), result.Err)
			} else {
				fmt.Fprintf(stderr, "unavailable %s: path was outside snapshot coverage\n", humanPath(result.Path))
			}
		default:
			exitCode = internalErrorExitCode
			restoreEvent.Status = "error"
			if result.Err != nil {
				restoreEvent.Error = result.Err.Error()
				fmt.Fprintf(stderr, "error     %s: %v; not restored\n", humanPath(result.Path), result.Err)
			} else {
				fmt.Fprintf(stderr, "error     %s; not restored\n", humanPath(result.Path))
			}
		}
		record.Files.RestoreEvents = append(record.Files.RestoreEvents, restoreEvent)
	}
	if err := store.SaveWhileSessionLocked(record); err != nil {
		fmt.Fprintf(stderr, "unring: record restore outcome: %v\n", err)
		return internalErrorExitCode
	}
	return exitCode
}

func snapshotsCommand(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("unring snapshots", flag.ContinueOnError)
	flags.SetOutput(stderr)
	showAll := flags.Bool("all", false, "show every recorded session, including sessions without restore data")
	flags.Usage = func() {
		fmt.Fprintln(stderr, "Usage: unring snapshots [--all]")
	}
	if err := flags.Parse(args); err != nil {
		return usageExitCode
	}
	if flags.NArg() != 0 {
		flags.Usage()
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
	for _, warning := range usage.Warnings {
		fmt.Fprintf(stderr, "unring: snapshot storage warning for %s: %s.\n", warning.SessionID, warning.Error)
	}
	if len(usage.Warnings) > 0 {
		fmt.Fprintf(stdout, "Snapshot storage: at least %d known bytes of %d bytes; %d sessions retained; usage is incomplete.\n",
			usage.Bytes, usage.CapBytes, usage.Sessions)
	} else if usage.Exact {
		fmt.Fprintf(stdout, "Snapshot storage: %d bytes used of %d bytes; %d sessions retained.\n",
			usage.Bytes, usage.CapBytes, usage.Sessions)
	} else {
		fmt.Fprintf(stdout, "Snapshot storage: upper-bound estimate %d bytes of %d bytes; %d sessions retained.\n",
			usage.Bytes, usage.CapBytes, usage.Sessions)
	}
	fmt.Fprintln(stdout, "The retained-session count is the number of clone stores present, not the number of audit records.")
	records, listErr := store.List()
	if listErr != nil {
		if records == nil {
			fmt.Fprintf(stderr, "unring: inspect recorded sessions: %v\n", listErr)
			return internalErrorExitCode
		}
		fmt.Fprintf(stderr, "unring: inspect session backstops: %v\n", listErr)
	}
	statuses := make([]snapshotSessionStatus, 0, len(records))
	withRestoreData := make([]snapshotSessionStatus, 0, len(records))
	for _, record := range records {
		status := inspectSnapshotSession(store.StateDir(), record)
		statuses = append(statuses, status)
		if status.hasRestoreData {
			withRestoreData = append(withRestoreData, status)
		}
	}
	shown := withRestoreData
	if *showAll {
		shown = statuses
	}
	shownCount, truncated := boundedHumanSessionCount(len(shown), *showAll)
	if truncated {
		shown = shown[:shownCount]
	}
	for _, status := range shown {
		fmt.Fprintln(stdout, status.line)
	}
	withoutRestoreData := len(statuses) - len(withRestoreData)
	if !*showAll && withoutRestoreData > 0 {
		fmt.Fprintf(stdout, "%d audit-only sessions with no currently restorable file snapshot data were omitted; use unring snapshots --all to show every recorded session.\n", withoutRestoreData)
	}
	if truncated {
		fmt.Fprintf(stdout, "Showing the newest %d of %d sessions with retained or possibly present restore data; use unring snapshots --all to show everything.\n", defaultSessionListLimit, len(withRestoreData))
	}
	return 0
}

type snapshotSessionStatus struct {
	line           string
	hasRestoreData bool
}

func inspectSnapshotSession(stateDir string, record audit.Record) snapshotSessionStatus {
	_, cloneErr := localrollback.LoadSealedSummary(stateDir, record.ID)
	cloneRetained := cloneErr == nil
	backstop := record.Files.Backstop
	if !backstop.Available {
		reason := backstop.Reason
		if reason == "" {
			reason = "no backstop was recorded"
		}
		if cloneRetained {
			return snapshotSessionStatus{
				line:           record.ID + "  CLONE STORE RETAINED; NO WHOLE-VOLUME BACKSTOP — " + reason,
				hasRestoreData: true,
			}
		}
		return snapshotSessionStatus{
			line: record.ID + "  NO RESTORABLE FILE SNAPSHOT DATA — clone store absent; no whole-volume backstop: " + reason,
		}
	}
	presences := localrollback.InspectBackstop(backstop)
	parts := make([]string, 0, len(presences))
	volumeMayRestore := false
	for _, presence := range presences {
		status := "present"
		if presence.Error != "" {
			status = "presence unknown: " + presence.Error
			volumeMayRestore = true
		} else if !presence.Present {
			status = "PURGED OR DELETED"
		} else {
			volumeMayRestore = true
		}
		parts = append(parts, presence.Snapshot.Name+" on "+presence.Snapshot.MountPoint+" — "+status)
	}
	prefix := "NO CLONE STORE"
	if cloneRetained {
		prefix = "CLONE STORE RETAINED"
	}
	line := record.ID + "  " + prefix
	if len(parts) > 0 {
		line += "; VOLUME SNAPSHOT: " + strings.Join(parts, "; ")
	} else {
		parts = append(parts, "recorded whole-volume backstop contains no snapshots")
		line += "; VOLUME BACKSTOP: " + parts[0]
	}
	if !cloneRetained && !volumeMayRestore {
		line = record.ID + "  NO RESTORABLE FILE SNAPSHOT DATA — clone store absent; " + strings.Join(parts, "; ")
	}
	return snapshotSessionStatus{line: line, hasRestoreData: cloneRetained || volumeMayRestore}
}

func boundedHumanSessionCount(total int, showAll bool) (int, bool) {
	if showAll || total <= defaultSessionListLimit {
		return total, false
	}
	return defaultSessionListLimit, true
}

func printRestoreListing(output io.Writer, record audit.Record) {
	fmt.Fprintf(output, "UNRING FILE CHANGES %s\n", record.ID)
	printStoredChangeListLimitation(output, record.Files, "  ", true)
	if record.Files.Interrupted {
		fmt.Fprintf(output, "INCOMPLETE: the post-session scan was interrupted; the paths below are only the changes recorded before cancellation, and this session must not be treated as clean. Run unring log --json %s for precise diagnostic details.\n", record.ID)
	} else if !record.Files.Complete {
		fmt.Fprintln(output, "INCOMPLETE: the post-session scan did not produce a complete change list; the paths below may omit changes.")
	}
	agentStateRoots, inferredAgentStateRoots := agentStateGroupingRoots(record.Files.AgentStateRoots)
	if inferredAgentStateRoots {
		printAgentStateGroupingInference(output, "", agentStateRoots)
	}
	regular, agentState := groupAgentStateChanges(record.Files.Changes, agentStateRoots)
	for _, change := range regular {
		printRestoreListingChange(output, change, record.Files.Retained, record.Files.Interrupted)
	}
	if len(agentState) > 0 {
		fmt.Fprintln(output, "AGENT OWN-STATE CHANGES — reported, but skipped by restore --all unless explicitly included")
		for _, change := range agentState {
			printRestoreListingChange(output, change, record.Files.Retained, record.Files.Interrupted)
		}
		if hasCurrentlyRestorableFileChange(agentState, record.Files.Retained) {
			fmt.Fprintf(output, "Include this group with: unring restore --all --include-agent-state %s\n", record.ID)
		}
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
	if hasCurrentlyRestorableFileChange(record.Files.Changes, record.Files.Retained) {
		fmt.Fprintf(output, "Restore selected paths with: unring restore %s <path> [...]\n", record.ID)
		fmt.Fprintf(output, "Restore every restorable path except agent own-state with: unring restore --all %s\n", record.ID)
	} else {
		fmt.Fprintln(output, "No changed path has restorable snapshot data.")
	}
}

func printRestoreListingChange(output io.Writer, change localrollback.Change, cloneRetained, interrupted bool) {
	fmt.Fprintf(output, "  %-8s %s\n", change.Kind, humanPath(change.Path))
	switch {
	case change.UnrestorableReason != "":
		fmt.Fprintf(output, "             NOT RESTORABLE: %s\n", humanUnrestorableReason(change.UnrestorableReason, interrupted))
	case change.RestoreSource == localrollback.RestoreSourceVolume:
		fmt.Fprintln(output, "             SNAPSHOT ONLY: restore requires sudo because APFS snapshot mounting is root-only")
	case !cloneRetained:
		fmt.Fprintln(output, "             NOT RESTORABLE: clone snapshot data was evicted; this path was not stored in the volume-snapshot restore layer")
	}
}

func loadStoredFileSummary(stateDir string, record audit.Record) localrollback.Summary {
	manifestSummary, err := localrollback.LoadSealedSummary(stateDir, record.ID)
	if err != nil {
		// Clone retention can evict the manifest before the audit record. The
		// audit copy also remains authoritative when the manifest is unsealed.
		summary := record.Files
		summary.Retained = false
		return summary
	}
	summary := record.Files
	// The audit record is the later, more durable copy of changes and errors.
	// Only the manifest's persisted disclosure fields are needed here.
	summary.ChangeListScope = manifestSummary.ChangeListScope
	summary.ChangeListRoots = append([]string(nil), manifestSummary.ChangeListRoots...)
	if len(summary.AgentStateRoots) == 0 {
		summary.AgentStateRoots = append([]string(nil), manifestSummary.AgentStateRoots...)
	}
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

func hasCurrentlyRestorableFileChange(changes []localrollback.Change, cloneRetained bool) bool {
	for _, change := range changes {
		if change.UnrestorableReason != "" {
			continue
		}
		if change.RestoreSource == localrollback.RestoreSourceVolume || cloneRetained {
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
	fmt.Fprintf(output, "Command:  %s\n", humanCommand(record.Command))
	fmt.Fprintf(output, "Decision: %s\n", record.Decision)
	fmt.Fprintf(output, "Outcome:  %s\n", displayedOutcomeDetail(record))
	fmt.Fprintf(output, "Exit code: %d\n", record.ExitCode)
	fmt.Fprintf(output, "Outbound interception: %t\n", record.Outbound)
	if record.Error != "" {
		fmt.Fprintf(output, "Error: %s\n", record.Error)
	}
	printAuditFiles(output, record.ID, record.Files)
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

func displayedOutcome(record audit.Record) string {
	if record.CompletionKind == completionKindAbnormalDiscard &&
		(record.ExitCode == 128+int(syscall.SIGINT) || record.ExitCode == 128+int(syscall.SIGTERM)) {
		if record.Outcome == "unknown" {
			return "interrupted (unconfirmed)"
		}
		return "interrupted"
	}
	if record.Outcome != "discarded" {
		return record.Outcome
	}
	switch record.CompletionKind {
	case completionKindNoDecision:
		return "no decision"
	case completionKindAbnormalDiscard:
		if record.ExitCode == 128+int(syscall.SIGINT) || record.ExitCode == 128+int(syscall.SIGTERM) {
			return "interrupted"
		}
		return "abnormal end"
	default:
		return "discarded"
	}
}

func displayedOutcomeDetail(record audit.Record) string {
	switch displayedOutcome(record) {
	case "no decision":
		return "no decision needed"
	case "interrupted":
		return "interrupted; reversible effects were discarded"
	case "interrupted (unconfirmed)":
		return "interrupted; final discard could not be confirmed"
	case "abnormal end":
		return "abnormal end; reversible effects were discarded"
	default:
		return displayedOutcome(record)
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
	interrupted <-chan struct{},
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
	case <-interrupted:
		fmt.Fprintln(output, "\nSignal received: discarding the session.")
		return pgproxy.DecisionRollback, true
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

func humanPath(path string) string {
	for _, character := range path {
		if character < ' ' || character == 0x7f {
			return strconv.Quote(path)
		}
	}
	return path
}

func humanCommand(command []string) string {
	displayed := make([]string, len(command))
	for index, argument := range command {
		displayed[index] = humanPath(argument)
	}
	return strings.Join(displayed, " ")
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
	fmt.Fprintln(output, "  unring log [--json] [--all] [session-id]")
	fmt.Fprintln(output, "  unring restore [--force] [--all [--include-agent-state]] <session-id> [changed-path ...]")
	fmt.Fprintln(output, "  unring prune [--all] [--confirm preview-token]")
	fmt.Fprintln(output, "  unring snapshots [--all]")
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
