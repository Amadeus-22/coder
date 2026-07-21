package chattool

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"time"

	"charm.land/fantasy"
	"github.com/google/uuid"
	"golang.org/x/xerrors"

	"cdr.dev/slog/v3"
	"github.com/coder/coder/v2/codersdk"
	"github.com/coder/coder/v2/codersdk/workspacesdk"
)

const (
	// defaultTimeout is the default timeout for command
	// execution.
	defaultTimeout = 10 * time.Second

	// maxOutputToModel is the maximum output sent to the LLM.
	maxOutputToModel = 32 << 10 // 32KB

	// snapshotTimeout is how long a non-blocking fallback
	// request is allowed to take when retrieving a process
	// output snapshot after a blocking wait times out.
	snapshotTimeout = 30 * time.Second

	// processWaitRetryDelay is the pause before re-issuing a
	// blocking process wait whose server-side bound returned
	// early, so a short agent-side wait cannot degenerate into a
	// zero-delay request loop.
	processWaitRetryDelay = time.Second

	// killSignalTimeout bounds the detached best-effort kill sent
	// when a user interrupt unwinds a foreground execute call.
	killSignalTimeout = 5 * time.Second
)

// ErrUserInterrupt is the cancellation cause chatd uses when a user
// action (interrupt, edit, or a message sent with interrupt)
// supersedes the running turn. The execute tool kills its foreground
// process only on this cause: every other cancellation (attempt
// watchdog timeout, worker shutdown, runner rebalancing) leaves the
// process running so a replay can re-attach through its idempotency
// token.
var ErrUserInterrupt = xerrors.New("chat turn superseded by a user action")

// ExecuteIdentity identifies one execute tool call across replays:
// the chat and the assistant message that issued the call, combined
// per dispatch with the provider tool call ID. History edits
// re-insert regenerated assistant messages under new IDs, so the
// triple never aliases a different call.
type ExecuteIdentity struct {
	ChatID             uuid.UUID
	AssistantMessageID int64
}

type executeIdentityContextKey struct{}

// WithExecuteIdentity returns a context carrying the identity the
// execute tool hashes into idempotency tokens for its process
// starts. Without it, starts carry no token and a replayed tool
// call runs its command again.
func WithExecuteIdentity(ctx context.Context, identity ExecuteIdentity) context.Context {
	return context.WithValue(ctx, executeIdentityContextKey{}, identity)
}

// clientTokenFromContext derives the idempotency token for a tool
// call, or "" when the identity or the tool call ID is missing:
// an empty tool call ID would alias every ID-less call in the
// message to one token.
func clientTokenFromContext(ctx context.Context, toolCallID string) string {
	identity, ok := ctx.Value(executeIdentityContextKey{}).(ExecuteIdentity)
	if !ok || identity.ChatID == uuid.Nil || identity.AssistantMessageID == 0 || toolCallID == "" {
		return ""
	}
	sum := sha256.Sum256(fmt.Appendf(nil, "%s|%d|%s", identity.ChatID, identity.AssistantMessageID, toolCallID))
	return hex.EncodeToString(sum[:])
}

// nonInteractiveEnvVars are set on every process to prevent
// interactive prompts that would hang a headless execution.
var nonInteractiveEnvVars = map[string]string{
	"GIT_EDITOR":          "true",
	"GIT_SEQUENCE_EDITOR": "true",
	"EDITOR":              "true",
	"VISUAL":              "true",
	"GIT_TERMINAL_PROMPT": "0",
	"NO_COLOR":            "1",
	"TERM":                "dumb",
	"PAGER":               "cat",
	"GIT_PAGER":           "cat",
}

// fileDumpPatterns detects commands that dump entire files.
// When matched, a note is added suggesting read_file instead.
var fileDumpPatterns = []*regexp.Regexp{
	regexp.MustCompile(`^cat\s+`),
	regexp.MustCompile(`^(rg|grep)\s+.*--include-all`),
	regexp.MustCompile(`^(rg|grep)\s+-l\s+`),
}

const (
	// shNotFoundFragment omits the trailing path variable
	// (%PATH% vs $PATH) for OS portability. Only transport
	// errors from StartProcess contain it, never command output.
	shNotFoundFragment = `exec: "sh": executable file not found`

	// shNotFoundGuidance is model-facing remediation text, relayed
	// to the user. Keep the docs anchor in sync with
	// docs/ai-coder/agents/architecture.md.
	shNotFoundGuidance = "The workspace has no POSIX shell (sh) on its PATH. " +
		"Coder Agents run commands with \"sh -c\". On Windows, install sh " +
		"via Git Bash, MSYS2, or WSL, then restart the workspace to pick " +
		"up the updated PATH. See " +
		"https://coder.com/docs/ai-coder/agents/architecture#windows-workspace-shell-requirement"
)

// enrichStartError appends actionable guidance when a StartProcess
// error indicates the workspace has no sh binary.
func enrichStartError(msg string) string {
	if strings.Contains(msg, shNotFoundFragment) {
		return msg + "\n\n" + shNotFoundGuidance
	}
	return msg
}

// ExecuteResult is the structured response from the execute
// tool.
type ExecuteResult struct {
	Success             bool                            `json:"success"`
	Output              string                          `json:"output,omitempty"`
	ExitCode            int                             `json:"exit_code"`
	WallDurationMs      int64                           `json:"wall_duration_ms"`
	Error               string                          `json:"error,omitempty"`
	Truncated           *workspacesdk.ProcessTruncation `json:"truncated,omitempty"`
	Note                string                          `json:"note,omitempty"`
	BackgroundProcessID string                          `json:"background_process_id,omitempty"`
}

// ExecuteOptions configures the execute tool.
type ExecuteOptions struct {
	GetWorkspaceConn func(context.Context) (workspacesdk.AgentConn, error)
	DefaultTimeout   time.Duration
	Logger           slog.Logger
}

// ProcessToolOptions configures a process management tool
// (process_output, process_list, or process_signal). Each of
// these tools only needs a workspace connection resolver.
type ProcessToolOptions struct {
	GetWorkspaceConn func(context.Context) (workspacesdk.AgentConn, error)
}

// ExecuteArgs are the parameters accepted by the execute tool.
type ExecuteArgs struct {
	Command         string  `json:"command" description:"The shell command to execute. Runs under \"sh -c\" (POSIX)."`
	ModelIntent     *string `json:"model_intent,omitempty" description:"A short, natural-language, present-participle phrase describing what you are doing. This is shown to the user alongside the command. Use plain English with no underscores or technical jargon. The UI appends \"using <command>\" and \"for <duration>\" automatically, so do not repeat the command or include a duration. Keep it under 100 characters. Good examples: \"Running the unit tests\", \"Checking repository state\", \"Inspecting build output\"."`
	Timeout         *string `json:"timeout,omitempty" description:"How long to wait for completion (e.g. '30s', '5m'). Default is 10s. The process keeps running if this expires and you get a background_process_id to re-attach. Only applies to foreground commands."`
	WorkDir         *string `json:"workdir,omitempty" description:"Working directory for the command."`
	RunInBackground *bool   `json:"run_in_background,omitempty" description:"Run without blocking. Use for persistent processes (dev servers, file watchers) or when you want to continue working while a command runs and check the result later with process_output. For commands whose result you need before continuing, prefer foreground with a longer timeout. Do NOT use shell & to background processes. It will not work correctly. Always use this parameter instead."`
}

// ExecuteToolName is the registered name of the execute tool.
const ExecuteToolName = "execute"

// Execute returns an AgentTool that runs a shell command in the
// workspace via the agent HTTP API.
func Execute(options ExecuteOptions) fantasy.AgentTool {
	return fantasy.NewAgentTool(
		ExecuteToolName,
		"Execute a shell command in the workspace. Runs under \"sh -c\" (POSIX). Waits for completion up to the timeout (default 10s, override with the timeout parameter e.g. '30s', '5m'). If the command exceeds the timeout, the response includes a background_process_id; use process_output with that ID to re-attach and wait for the result. Use run_in_background=true for persistent processes (dev servers, file watchers) or when you want to continue other work while the command runs. Never use shell '&' for backgrounding.",
		func(ctx context.Context, args ExecuteArgs, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
			if options.GetWorkspaceConn == nil {
				return fantasy.NewTextErrorResponse("workspace connection resolver is not configured"), nil
			}
			conn, err := options.GetWorkspaceConn(ctx)
			if err != nil {
				return fantasy.NewTextErrorResponse(err.Error()), nil
			}
			return executeTool(ctx, conn, args, options, call.ID), nil
		},
	)
}

func executeTool(
	ctx context.Context,
	conn workspacesdk.AgentConn,
	args ExecuteArgs,
	options ExecuteOptions,
	toolCallID string,
) fantasy.ToolResponse {
	if args.Command == "" {
		return fantasy.NewTextErrorResponse("command is required")
	}

	clientToken := clientTokenFromContext(ctx, toolCallID)

	// Build the environment map for the process request.
	env := make(map[string]string, len(nonInteractiveEnvVars)+1)
	env["CODER_CHAT_AGENT"] = "true"
	for k, v := range nonInteractiveEnvVars {
		env[k] = v
	}

	background := args.RunInBackground != nil && *args.RunInBackground

	// Detect shell-style backgrounding (trailing &) and promote to
	// background mode. Models sometimes use "cmd &" instead of the
	// run_in_background parameter, which causes the shell to fork
	// and exit immediately, leaving an untracked orphan process.
	trimmed := strings.TrimSpace(args.Command)
	if !background && strings.HasSuffix(trimmed, "&") && !strings.HasSuffix(trimmed, "&&") && !strings.HasSuffix(trimmed, "|&") {
		background = true
		args.Command = strings.TrimSpace(strings.TrimSuffix(trimmed, "&"))
	}

	var workDir string
	if args.WorkDir != nil {
		workDir = *args.WorkDir
	}

	if background {
		return executeBackground(ctx, conn, options, args.Command, workDir, env, clientToken)
	}
	return executeForeground(ctx, conn, options, args, workDir, env, clientToken)
}

// isConflictError reports a 409 from the agent: the idempotency
// token was already used to start a process with different
// parameters, or waiting out a concurrent start owning the token
// was aborted.
func isConflictError(err error) bool {
	var sdkErr *codersdk.Error
	return xerrors.As(err, &sdkErr) && sdkErr.StatusCode() == http.StatusConflict
}

// isTokenWaitAbortedError reports the aborted-wait flavor of the
// agent's 409: the start that owns this token had not published a
// process before the wait gave up, so the dispatch outcome is
// unresolved rather than conflicted. Matches the sentinel text of
// agentproc's errTokenWaitAborted, which every aborted-wait detail
// carries; if the text ever drifts, this degrades to the permanent
// conflict handling instead of misclassifying errors.
func isTokenWaitAbortedError(err error) bool {
	var sdkErr *codersdk.Error
	if !xerrors.As(err, &sdkErr) || sdkErr.StatusCode() != http.StatusConflict {
		return false
	}
	return strings.Contains(sdkErr.Detail, "aborted waiting for the concurrent start")
}

// startProcessResolvingTokenWait issues StartProcess and retries
// aborted-wait conflicts while ctx lasts. The start owning the
// token either publishes its process (the retry attaches) or
// releases the token (the retry starts fresh), so retrying
// resolves an outcome that would otherwise be recorded as a
// permanent synthetic failure.
func startProcessResolvingTokenWait(ctx context.Context, conn workspacesdk.AgentConn, req workspacesdk.StartProcessRequest) (workspacesdk.StartProcessResponse, error) {
	for {
		resp, err := conn.StartProcess(ctx, req)
		if err == nil || req.ClientToken == "" || !isTokenWaitAbortedError(err) {
			return resp, err
		}
		select {
		case <-ctx.Done():
			return resp, err
		case <-time.After(processWaitRetryDelay):
		}
	}
}

func isNotFoundError(err error) bool {
	var sdkErr *codersdk.Error
	return xerrors.As(err, &sdkErr) && sdkErr.StatusCode() == http.StatusNotFound
}

// killOnUserInterrupt best-effort kills a foreground process whose
// wait unwound because the user superseded the turn. It is a no-op
// for every other cancellation cause. The signal rides a detached,
// bounded context because the tool context is already canceled. A
// 404 (process unknown) or 409 (already exited) answer means the
// process is already gone; other failures are logged and dropped,
// leaving the process visible and killable in the process list.
func killOnUserInterrupt(ctx context.Context, conn workspacesdk.AgentConn, processID string, logger slog.Logger) {
	if !xerrors.Is(context.Cause(ctx), ErrUserInterrupt) {
		return
	}
	killCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), killSignalTimeout)
	defer cancel()
	err := conn.SignalProcess(killCtx, processID, "kill")
	if err == nil || isNotFoundError(err) || isConflictError(err) {
		return
	}
	logger.Warn(ctx, "failed to kill foreground process on user interrupt",
		slog.F("process_id", processID),
		slog.Error(err),
	)
}

// startConflictResult converts a token conflict into an error
// result. A parameter mismatch is permanent: the token's earlier
// start recorded different parameters, and no new process was
// started. An aborted wait (budget exhausted in
// startProcessResolvingTokenWait) is unresolved: the owning start
// may still publish and run the command.
func startConflictResult(ctx context.Context, logger slog.Logger, clientToken string, err error) fantasy.ToolResponse {
	logger.Warn(ctx, "execute start conflicted on its idempotency token",
		slog.F("client_token", clientToken),
		slog.Error(err),
	)
	if isTokenWaitAbortedError(err) {
		return errorResult(fmt.Sprintf(
			"start process: %v. The outcome is unresolved: a concurrent dispatch of this tool call owns the command and may still run it. Check the process list before re-running the command.", err,
		))
	}
	return errorResult(fmt.Sprintf(
		"start process: %v. This dispatch conflicted with an earlier start of the same tool call with different parameters; no new process was started.", err,
	))
}

// executeBackground starts a process in the background and
// returns immediately with the process ID. Attaching to a process
// an earlier dispatch of the same token started yields the same
// result, so replays need no special handling here.
func executeBackground(
	ctx context.Context,
	conn workspacesdk.AgentConn,
	options ExecuteOptions,
	command string,
	workDir string,
	env map[string]string,
	clientToken string,
) fantasy.ToolResponse {
	resp, err := startProcessResolvingTokenWait(ctx, conn, workspacesdk.StartProcessRequest{
		Command:     command,
		WorkDir:     workDir,
		Env:         env,
		Background:  true,
		ClientToken: clientToken,
	})
	if err != nil {
		if clientToken != "" && isConflictError(err) {
			return startConflictResult(ctx, options.Logger, clientToken, err)
		}
		return errorResult(enrichStartError(fmt.Sprintf("start background process: %v", err)))
	}
	KickAttemptKeepalive(ctx)

	result := ExecuteResult{
		Success:             true,
		BackgroundProcessID: resp.ID,
	}
	data, err := json.Marshal(result)
	if err != nil {
		return fantasy.NewTextErrorResponse(err.Error())
	}
	return fantasy.NewTextResponse(string(data))
}

// executeForeground starts a process and waits for its
// completion, enforcing the configured timeout.
func executeForeground(
	ctx context.Context,
	conn workspacesdk.AgentConn,
	options ExecuteOptions,
	args ExecuteArgs,
	workDir string,
	env map[string]string,
	clientToken string,
) fantasy.ToolResponse {
	timeout := options.DefaultTimeout
	if timeout <= 0 {
		timeout = defaultTimeout
	}
	if args.Timeout != nil {
		parsed, err := time.ParseDuration(*args.Timeout)
		if err != nil {
			return fantasy.NewTextErrorResponse(
				fmt.Sprintf("invalid timeout %q: %v", *args.Timeout, err),
			)
		}
		timeout = parsed
	}

	cmdCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	start := time.Now()

	resp, err := startProcessResolvingTokenWait(cmdCtx, conn, workspacesdk.StartProcessRequest{
		Command:     args.Command,
		WorkDir:     workDir,
		Env:         env,
		Background:  false,
		ClientToken: clientToken,
	})
	if err != nil {
		if clientToken != "" && isConflictError(err) {
			return startConflictResult(ctx, options.Logger, clientToken, err)
		}
		return errorResult(enrichStartError(fmt.Sprintf("start process: %v", err)))
	}
	KickAttemptKeepalive(ctx)

	var result ExecuteResult
	if resp.Attached {
		options.Logger.Debug(ctx, "execute attached to a process from an earlier dispatch",
			slog.F("process_id", resp.ID),
			slog.F("client_token", clientToken),
		)
		result = resumeAttachedProcess(cmdCtx, ctx, conn, resp.ID, resp.StartedAt, timeout)
		if resp.StartedAt > 0 {
			// The command has been running since the earlier
			// dispatch; anchoring the wall duration there keeps
			// it truthful across replays.
			start = time.Unix(resp.StartedAt, 0)
		}
	} else {
		result = waitForProcess(cmdCtx, ctx, conn, resp.ID, timeout)
	}
	if result.BackgroundProcessID != "" {
		// The wait ended without observing an exit; a command a
		// user interrupt abandoned is unwanted, so kill it.
		killOnUserInterrupt(ctx, conn, resp.ID, options.Logger)
	}
	// An attached start anchored to an agent clock that runs ahead
	// can place start in the future; clamp instead of reporting a
	// negative duration.
	result.WallDurationMs = max(time.Since(start).Milliseconds(), 0)

	// Add an advisory note for file-dump commands.
	if note := detectFileDump(args.Command); note != "" {
		result.Note = note
	}

	data, err := json.Marshal(result)
	if err != nil {
		return fantasy.NewTextErrorResponse(err.Error())
	}
	return fantasy.NewTextResponse(string(data))
}

// resumeAttachedProcess waits out the remainder of the original
// execution budget of a process that an earlier dispatch of the
// same tool call started, bounded by both the agent-reported
// remaining budget and cmdCtx. An exited process reports its
// real result even past the deadline: the work already happened.
// A running process past its deadline detaches with the standard
// timed-out result instead of a fresh budget on every replay.
func resumeAttachedProcess(
	cmdCtx context.Context,
	ctx context.Context,
	conn workspacesdk.AgentConn,
	processID string,
	startedAtUnix int64,
	timeout time.Duration,
) ExecuteResult {
	// A zero start time (agent predating the field) leaves no way
	// to compute the remaining budget; resolve from a snapshot so
	// the replay cannot wait a fresh full timeout.
	remaining := time.Duration(0)
	if startedAtUnix > 0 {
		remaining = time.Until(time.Unix(startedAtUnix, 0).Add(timeout))
		// StartedAt comes from the agent's clock; clamp so skew
		// can never grant a replay more than the requested timeout.
		remaining = min(remaining, timeout)
	}
	if remaining <= 0 {
		bgCtx, cancel := context.WithTimeout(ctx, snapshotTimeout)
		defer cancel()
		resp, err := conn.ProcessOutput(bgCtx, processID, nil)
		if err != nil {
			return ExecuteResult{
				Success:             false,
				ExitCode:            -1,
				Error:               fmt.Sprintf("get attached process output: %v; use process_output with ID %s to retry", err, processID),
				BackgroundProcessID: processID,
			}
		}
		KickAttemptKeepalive(ctx)
		if resp.Running {
			return timedOutRunningResult(resp, timeout, processID)
		}
		return completedResult(resp)
	}
	waitCtx, cancel := context.WithTimeout(cmdCtx, remaining)
	defer cancel()
	return waitForProcess(waitCtx, ctx, conn, processID, timeout)
}

// truncateOutput safely truncates output to maxOutputToModel,
// ensuring the result is valid UTF-8 even if the cut falls in
// the middle of a multi-byte character.
func truncateOutput(output string) string {
	if len(output) > maxOutputToModel {
		output = strings.ToValidUTF8(output[:maxOutputToModel], "")
	}
	return output
}

// timedOutRunningResult builds the ExecuteResult for a process that
// is still running when the caller's timeout expires: partial output
// plus the process handle so the model can re-attach or poll.
func timedOutRunningResult(resp workspacesdk.ProcessOutputResponse, timeout time.Duration, processID string) ExecuteResult {
	return ExecuteResult{
		Success:             false,
		Output:              truncateOutput(resp.Output),
		ExitCode:            -1,
		Error:               fmt.Sprintf("command timed out after %s", timeout),
		Truncated:           resp.Truncated,
		BackgroundProcessID: processID,
	}
}

// completedResult builds the ExecuteResult for a process output
// snapshot, treating a missing exit code as success. Callers ensure
// resp describes an exited process.
func completedResult(resp workspacesdk.ProcessOutputResponse) ExecuteResult {
	exitCode := 0
	if resp.ExitCode != nil {
		exitCode = *resp.ExitCode
	}
	return ExecuteResult{
		Success:   exitCode == 0,
		Output:    truncateOutput(resp.Output),
		ExitCode:  exitCode,
		Truncated: resp.Truncated,
	}
}

// waitForProcess blocks until the process exits or the context
// expires. On any error (timeout or transport), it tries a
// non-blocking snapshot to recover. Total wall time may exceed
// timeout by up to snapshotTimeout if recovery is needed.
func waitForProcess(
	ctx context.Context,
	parentCtx context.Context,
	conn workspacesdk.AgentConn,
	processID string,
	timeout time.Duration,
) ExecuteResult {
	for {
		resp, err := conn.ProcessOutput(ctx, processID, &workspacesdk.ProcessOutputOptions{
			Wait: true,
		})
		if err == nil {
			KickAttemptKeepalive(parentCtx)
		}
		if err == nil && resp.Running && ctx.Err() == nil {
			// The server-side wait can return before the process
			// exits when its maximum wait is shorter than the
			// caller's timeout.
			select {
			case <-ctx.Done():
			case <-time.After(processWaitRetryDelay):
			}
			if ctx.Err() == nil {
				continue
			}
		}
		if err == nil && resp.Running && ctx.Err() != nil {
			// The deadline expired after a successful early
			// return, so resp predates the retry delay. Refresh
			// it so an exit or output during that window is
			// reported instead of the stale snapshot.
			resp = refreshRunningSnapshot(parentCtx, conn, processID, resp)
		}
		return resolveProcessWait(ctx, parentCtx, conn, processID, timeout, resp, err)
	}
}

// refreshRunningSnapshot re-reads a running process with a fresh
// non-blocking snapshot on the parent context. It returns the
// original response when the snapshot fails; the caller resolves
// from the best snapshot available.
func refreshRunningSnapshot(
	parentCtx context.Context,
	conn workspacesdk.AgentConn,
	processID string,
	resp workspacesdk.ProcessOutputResponse,
) workspacesdk.ProcessOutputResponse {
	bgCtx, bgCancel := context.WithTimeout(parentCtx, snapshotTimeout)
	defer bgCancel()
	fresh, err := conn.ProcessOutput(bgCtx, processID, nil)
	if err != nil {
		return resp
	}
	return fresh
}

// resolveProcessWait turns the final blocking-wait response (or
// error) into the ExecuteResult returned to the model.
func resolveProcessWait(
	ctx context.Context,
	parentCtx context.Context,
	conn workspacesdk.AgentConn,
	processID string,
	timeout time.Duration,
	resp workspacesdk.ProcessOutputResponse,
	err error,
) ExecuteResult {
	if err != nil {
		origErr := err
		timedOut := ctx.Err() != nil

		// Fetch a snapshot with a fresh context. The blocking
		// request may have failed due to a context timeout or
		// a transport error (e.g. the server's WriteTimeout
		// killed the connection). Either way, the process may
		// still have output available.
		bgCtx, bgCancel := context.WithTimeout(
			parentCtx,
			snapshotTimeout,
		)
		defer bgCancel()
		resp, err = conn.ProcessOutput(bgCtx, processID, nil)
		if err != nil {
			errMsg := fmt.Sprintf("get process output: %v; use process_output with ID %s to retry", origErr, processID)
			if timedOut {
				errMsg = fmt.Sprintf("command timed out after %s; failed to get output: %v", timeout, err)
			}
			return ExecuteResult{
				Success:             false,
				ExitCode:            -1,
				Error:               errMsg,
				BackgroundProcessID: processID,
			}
		}

		KickAttemptKeepalive(parentCtx)

		// Snapshot succeeded. If the process finished, return
		// its real result (transparent recovery).
		if !resp.Running {
			return completedResult(resp)
		}

		// Process still running, return partial output.
		if timedOut {
			return timedOutRunningResult(resp, timeout, processID)
		}
		return ExecuteResult{
			Success:             false,
			Output:              truncateOutput(resp.Output),
			ExitCode:            -1,
			Error:               fmt.Sprintf("get process output: %v (process still running, use process_output to check later)", origErr),
			Truncated:           resp.Truncated,
			BackgroundProcessID: processID,
		}
	}

	if resp.Running {
		// Only reachable once ctx ended while the process was
		// still running; waitForProcess retries early server-side
		// wait returns itself.
		return timedOutRunningResult(resp, timeout, processID)
	}

	return completedResult(resp)
}

// errorResult builds a ToolResponse from an ExecuteResult with
// an error message.
func errorResult(msg string) fantasy.ToolResponse {
	data, err := json.Marshal(ExecuteResult{
		Success: false,
		Error:   msg,
	})
	if err != nil {
		return fantasy.NewTextErrorResponse(msg)
	}
	return fantasy.NewTextResponse(string(data))
}

// detectFileDump checks whether the command matches a file-dump
// pattern and returns an advisory note, or empty string if no
// match.
func detectFileDump(command string) string {
	for _, pat := range fileDumpPatterns {
		if pat.MatchString(command) {
			return "Consider using read_file instead of " +
				"dumping file contents with shell commands."
		}
	}
	return ""
}

const (
	// defaultProcessOutputTimeout is the default time the
	// process_output tool blocks waiting for new output or
	// process exit before returning. This avoids polling
	// loops that waste tokens and HTTP round-trips.
	defaultProcessOutputTimeout = 10 * time.Second
)

// ProcessOutputArgs are the parameters accepted by the
// process_output tool.
type ProcessOutputArgs struct {
	ProcessID   string  `json:"process_id"`
	WaitTimeout *string `json:"wait_timeout,omitempty" description:"Override the default 10s block duration. The call blocks until the process exits or this timeout is reached. Set to '0s' for an immediate snapshot without waiting."`
}

// ProcessOutput returns an AgentTool that retrieves the output
// of a tracked process by its ID.
func ProcessOutput(options ProcessToolOptions) fantasy.AgentTool {
	return fantasy.NewAgentTool(
		"process_output",
		"Retrieve output from a tracked process by ID. "+
			"Use the process_id returned by execute with "+
			"run_in_background=true or from a timed-out "+
			"execute's background_process_id. Blocks up to "+
			"10s for the process to exit, then returns the "+
			"output and exit_code. If still running after "+
			"the timeout, returns the output so far. Use "+
			"wait_timeout to override the default 10s wait "+
			"(e.g. '30s', or '0s' for an immediate snapshot "+
			"without waiting).",
		func(ctx context.Context, args ProcessOutputArgs, _ fantasy.ToolCall) (fantasy.ToolResponse, error) {
			if options.GetWorkspaceConn == nil {
				return fantasy.NewTextErrorResponse("workspace connection resolver is not configured"), nil
			}
			if args.ProcessID == "" {
				return fantasy.NewTextErrorResponse("process_id is required"), nil
			}
			conn, err := options.GetWorkspaceConn(ctx)
			if err != nil {
				return fantasy.NewTextErrorResponse(err.Error()), nil
			}

			timeout := defaultProcessOutputTimeout
			if args.WaitTimeout != nil {
				parsed, err := time.ParseDuration(*args.WaitTimeout)
				if err != nil {
					return fantasy.NewTextErrorResponse(
						fmt.Sprintf("invalid wait_timeout %q: %v", *args.WaitTimeout, err),
					), nil
				}
				timeout = parsed
			}
			var opts *workspacesdk.ProcessOutputOptions
			// Save parent context before applying timeout.
			parentCtx := ctx
			if timeout > 0 {
				opts = &workspacesdk.ProcessOutputOptions{
					Wait: true,
				}
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, timeout)
				defer cancel()
			}
			resp, err := conn.ProcessOutput(ctx, args.ProcessID, opts)
			if err != nil {
				// The blocking request may have failed due to a
				// context timeout or a transport error (e.g.
				// server WriteTimeout). Try a non-blocking
				// snapshot if the parent context is still alive.
				if parentCtx.Err() != nil {
					return errorResult(fmt.Sprintf("get process output: %v", err)), nil
				}
				bgCtx, bgCancel := context.WithTimeout(parentCtx, snapshotTimeout)
				defer bgCancel()
				resp, err = conn.ProcessOutput(bgCtx, args.ProcessID, nil)
				if err != nil {
					return errorResult(fmt.Sprintf("get process output: %v", err)), nil
				}
				// Fall through to normal response handling below.
			}
			// Each successful poll proves the agent is responsive;
			// a wait longer than the attempt idle window must not
			// trip the watchdog while the agent responds.
			KickAttemptKeepalive(parentCtx)
			output := truncateOutput(resp.Output)
			exitCode := 0
			if resp.ExitCode != nil {
				exitCode = *resp.ExitCode
			}
			result := ExecuteResult{
				Success:   !resp.Running && exitCode == 0,
				Output:    output,
				ExitCode:  exitCode,
				Truncated: resp.Truncated,
			}
			if resp.Running {
				// Process is still running, success is not
				// yet determined.
				result.Success = true
				result.Note = "process is still running"
			}
			data, err := json.Marshal(result)
			if err != nil {
				return fantasy.NewTextErrorResponse(err.Error()), nil
			}
			return fantasy.NewTextResponse(string(data)), nil
		},
	)
}

// ProcessList returns an AgentTool that lists all tracked
// processes on the workspace agent.
func ProcessList(options ProcessToolOptions) fantasy.AgentTool {
	return fantasy.NewAgentTool(
		"process_list",
		"List all tracked processes in the workspace. "+
			"Returns process IDs, commands, status (running or "+
			"exited), and exit codes. Use this to discover "+
			"processes or check which are still running.",
		func(ctx context.Context, _ struct{}, _ fantasy.ToolCall) (fantasy.ToolResponse, error) {
			if options.GetWorkspaceConn == nil {
				return fantasy.NewTextErrorResponse("workspace connection resolver is not configured"), nil
			}
			conn, err := options.GetWorkspaceConn(ctx)
			if err != nil {
				return fantasy.NewTextErrorResponse(err.Error()), nil
			}
			resp, err := conn.ListProcesses(ctx)
			if err != nil {
				return errorResult(fmt.Sprintf("list processes: %v", err)), nil
			}
			data, err := json.Marshal(resp)
			if err != nil {
				return fantasy.NewTextErrorResponse(err.Error()), nil
			}
			return fantasy.NewTextResponse(string(data)), nil
		},
	)
}

// ProcessSignalArgs are the parameters accepted by the
// process_signal tool.
type ProcessSignalArgs struct {
	ProcessID string `json:"process_id"`
	Signal    string `json:"signal"`
}

// ProcessSignal returns an AgentTool that sends a signal to a
// tracked process on the workspace agent by its ID.
func ProcessSignal(options ProcessToolOptions) fantasy.AgentTool {
	return fantasy.NewAgentTool(
		"process_signal",
		"Send a signal to a tracked process. "+
			"Use \"terminate\" (SIGTERM) for graceful shutdown "+
			"or \"kill\" (SIGKILL) to force stop. Use the "+
			"process_id returned by execute with "+
			"run_in_background=true or from process_list.",
		func(ctx context.Context, args ProcessSignalArgs, _ fantasy.ToolCall) (fantasy.ToolResponse, error) {
			if options.GetWorkspaceConn == nil {
				return fantasy.NewTextErrorResponse("workspace connection resolver is not configured"), nil
			}
			if args.ProcessID == "" {
				return fantasy.NewTextErrorResponse("process_id is required"), nil
			}
			if args.Signal != "terminate" && args.Signal != "kill" {
				return fantasy.NewTextErrorResponse(
					"signal must be \"terminate\" or \"kill\"",
				), nil
			}
			conn, err := options.GetWorkspaceConn(ctx)
			if err != nil {
				return fantasy.NewTextErrorResponse(err.Error()), nil
			}
			if err := conn.SignalProcess(ctx, args.ProcessID, args.Signal); err != nil {
				return errorResult(fmt.Sprintf("signal process: %v", err)), nil
			}
			data, err := json.Marshal(map[string]any{
				"success": true,
				"message": fmt.Sprintf(
					"signal %q sent to process %s",
					args.Signal, args.ProcessID,
				),
			})
			if err != nil {
				return fantasy.NewTextErrorResponse(err.Error()), nil
			}
			return fantasy.NewTextResponse(string(data)), nil
		},
	)
}
