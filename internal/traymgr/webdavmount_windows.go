//go:build windows

package traymgr

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

var procCreateProcessWithTokenW = windows.NewLazySystemDLL("advapi32.dll").NewProc("CreateProcessWithTokenW")

func mountWebDAVNative(ctx context.Context, req webDAVMountRequest) error {
	ctx, cancel := withMountTimeout(ctx)
	defer cancel()
	// WebClient may be stopped after reboot but trigger-start when net use opens
	// the WebDAV remote; keep explicit start failures as context, not a hard stop.
	webClientErr := ensureWindowsWebClient(ctx)
	remotes, err := windowsWebDAVRemotes(req.URL)
	if err != nil {
		return err
	}
	drive := req.Drive
	if strings.TrimSpace(drive) == "" {
		drive = "*"
	}
	uses, _, err := windowsAllDriveUsesWithOutput(ctx)
	if err != nil {
		return err
	}
	if use, ok, err := findUsableWindowsWebDAVMapping(ctx, uses, remotes, "*"); err != nil {
		return err
	} else if ok {
		if drive == "*" || strings.EqualFold(use.Local, drive) {
			return nil
		}
		return fmt.Errorf("webdav is already mounted on %s; configured drive is %s", use.Local, drive)
	}
	if drive == "*" {
		for _, use := range uses {
			if use.Local != "" && windowsRemoteLooksLikeCNCWebDAV(use.Remote) {
				if err := unmountWindowsDrive(ctx, use.Local); err != nil {
					return err
				}
			}
		}
	}
	if drive != "*" {
		status, err := windowsDriveStatus(ctx, drive)
		if err != nil {
			return err
		}
		if status.Mapped {
			if windowsRemoteLooksLikeCNCWebDAV(status.Remote) {
				if err := unmountWindowsDrive(ctx, drive); err != nil {
					return err
				}
			} else {
				return fmt.Errorf("configured WebDAV drive %s is already mapped to %s", drive, status.Remote)
			}
		} else if exists, err := windowsDrivePathExists(ctx, drive); err != nil {
			return err
		} else if exists {
			return fmt.Errorf("configured WebDAV drive %s is already in use", drive)
		}
	}
	var errs []error
	if webClientErr != nil {
		errs = append(errs, webClientErr)
	}
	for _, remote := range remotes {
		if out, err := windowsNetUse(ctx, drive, remote, req.User, req.Password); err == nil {
			uses, listOut, err := windowsAllDriveUsesWithOutput(ctx)
			if err != nil {
				errs = append(errs, err)
				continue
			}
			if _, ok, err := findUsableWindowsWebDAVMapping(ctx, uses, remotes, drive); err != nil {
				errs = append(errs, err)
				continue
			} else if ok {
				return nil
			}
			verifyErr := fmt.Errorf("mount webdav %s exited successfully but no matching drive mapping is visible; command output: %s; net use output: %s", remote, emptyText(out), emptyText(listOut))
			if drive != "*" {
				return errors.Join(webClientErr, verifyErr)
			}
			errs = append(errs, verifyErr)
		} else {
			if uses, _, listErr := windowsAllDriveUsesWithOutput(ctx); listErr == nil {
				if _, ok, verifyErr := findUsableWindowsWebDAVMapping(ctx, uses, remotes, drive); verifyErr == nil && ok {
					return nil
				}
			}
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func windowsNetUse(ctx context.Context, drive, remote, user, password string) (string, error) {
	args := []string{"use", drive, remote}
	if password != "" {
		args = append(args, "*")
	}
	if user != "" && password != "" {
		args = append(args, "/user:"+user)
	}
	args = append(args, "/persistent:yes")
	out, err := windowsRunMountCommand(ctx, "net", args...)
	if err != nil {
		return out, fmt.Errorf("mount webdav %s: %w: %s", remote, err, strings.TrimSpace(out))
	}
	return out, nil
}

func unmountWebDAVNative(ctx context.Context, req webDAVMountRequest) error {
	ctx, cancel := withMountTimeout(ctx)
	defer cancel()
	drive := req.Drive
	if strings.TrimSpace(drive) == "" {
		drive = "*"
	}
	if drive == "*" {
		return unmountWindowsWebDAVRemotes(ctx, req.URL)
	}
	return unmountWindowsDrive(ctx, drive)
}

func unmountWindowsWebDAVRemotes(ctx context.Context, rawURL string) error {
	remotes, err := windowsWebDAVRemotes(rawURL)
	if err != nil {
		return err
	}
	uses, err := windowsAllDriveUses(ctx)
	if err != nil {
		return err
	}
	var errs []error
	found := false
	for _, use := range uses {
		if use.Local != "" && windowsRemoteMatches(use.Remote, remotes) {
			found = true
			if err := unmountWindowsDrive(ctx, use.Local); err != nil {
				errs = append(errs, err)
			}
		}
	}
	if !found {
		return nil
	}
	return errors.Join(errs...)
}

func unmountWindowsDrive(ctx context.Context, drive string) error {
	out, err := windowsRunMountCommand(ctx, "net", "use", drive, "/delete", "/yes")
	if err != nil {
		text := strings.ToLower(out)
		if strings.Contains(text, "not found") || strings.Contains(text, "could not be found") || strings.Contains(text, "does not exist") {
			return nil
		}
		return fmt.Errorf("unmount webdav: %w: %s", err, strings.TrimSpace(out))
	}
	return nil
}

type windowsDriveUse struct {
	Mapped bool
	Status string
	Local  string
	Remote string
}

func windowsDriveStatus(ctx context.Context, drive string) (windowsDriveUse, error) {
	out, err := windowsRunMountCommand(ctx, "net", "use", drive)
	text := strings.TrimSpace(out)
	if err != nil {
		lower := strings.ToLower(text)
		if strings.Contains(lower, "not found") ||
			strings.Contains(lower, "could not be found") ||
			strings.Contains(lower, "does not exist") ||
			strings.Contains(lower, "there are no entries") {
			return windowsDriveUse{}, nil
		}
		return windowsDriveUse{}, fmt.Errorf("check drive %s mapping: %w: %s", drive, err, text)
	}
	return parseWindowsDriveUse(text), nil
}

func parseWindowsDriveUse(text string) windowsDriveUse {
	st := windowsDriveUse{}
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if strings.Contains(strings.ToLower(line), "there are no entries") {
			return windowsDriveUse{}
		}
		if value, ok := windowsUseDetailValue(line, "local name"); ok {
			st.Local = value
		}
		if value, ok := windowsUseDetailValue(line, "status"); ok {
			st.Status = value
		}
		if value, ok := windowsUseDetailValue(line, "remote name"); ok {
			st.Remote = value
		}
	}
	st.Mapped = st.Remote != ""
	return st
}

func windowsUseDetailValue(line, key string) (string, bool) {
	if !strings.HasPrefix(strings.ToLower(line), key) {
		return "", false
	}
	return strings.TrimSpace(line[len(key):]), true
}

func windowsAllDriveUses(ctx context.Context) ([]windowsDriveUse, error) {
	uses, _, err := windowsAllDriveUsesWithOutput(ctx)
	return uses, err
}

func windowsAllDriveUsesWithOutput(ctx context.Context) ([]windowsDriveUse, string, error) {
	out, err := windowsRunMountCommand(ctx, "net", "use")
	text := strings.TrimSpace(out)
	if err != nil {
		lower := strings.ToLower(text)
		if strings.Contains(lower, "there are no entries") {
			return nil, text, nil
		}
		return nil, text, fmt.Errorf("list Windows drive mappings: %w: %s", err, text)
	}
	return parseWindowsAllDriveUses(text), text, nil
}

func parseWindowsAllDriveUses(text string) []windowsDriveUse {
	var out []windowsDriveUse
	for _, line := range strings.Split(text, "\n") {
		fields := strings.Fields(strings.TrimSpace(line))
		if len(fields) < 3 {
			continue
		}
		localIdx := -1
		for i, field := range fields {
			if len(field) == 2 && field[1] == ':' {
				localIdx = i
				break
			}
		}
		if localIdx < 0 || localIdx+1 >= len(fields) {
			continue
		}
		status := ""
		if localIdx > 0 {
			status = fields[0]
		}
		out = append(out, windowsDriveUse{Mapped: true, Status: status, Local: fields[localIdx], Remote: fields[localIdx+1]})
	}
	return out
}

func ensureWindowsWebClient(ctx context.Context) error {
	running, detail, err := windowsWebClientRunning(ctx)
	if err != nil {
		return err
	}
	if running {
		return nil
	}
	cmd := exec.CommandContext(ctx, "net", "start", "WebClient")
	configureBackgroundCommand(cmd)
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	if err := cmd.Run(); err != nil {
		text := strings.TrimSpace(out.String())
		if text == "" {
			text = detail
		}
		return fmt.Errorf("Windows WebClient service is required for WebDAV mounts but is not running, and starting it failed: %w: %s; start it once from an elevated PowerShell with: Set-Service WebClient -StartupType Automatic; Start-Service WebClient", err, text)
	}
	running, detail, err = windowsWebClientRunning(ctx)
	if err != nil {
		return err
	}
	if !running {
		return fmt.Errorf("Windows WebClient service did not reach RUNNING state: %s; start it once from an elevated PowerShell with: Set-Service WebClient -StartupType Automatic; Start-Service WebClient", detail)
	}
	return nil
}

func windowsWebClientRunning(ctx context.Context) (bool, string, error) {
	cmd := exec.CommandContext(ctx, "sc", "query", "WebClient")
	configureBackgroundCommand(cmd)
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	err := cmd.Run()
	text := strings.TrimSpace(out.String())
	if err != nil {
		return false, text, fmt.Errorf("check Windows WebClient service: %w: %s", err, text)
	}
	return strings.Contains(strings.ToUpper(text), "RUNNING"), text, nil
}

func webDAVMountedNative(ctx context.Context, req webDAVMountRequest) (bool, error) {
	ctx, cancel := withMountTimeout(ctx)
	defer cancel()
	drive := req.Drive
	if strings.TrimSpace(drive) == "" {
		drive = "*"
	}
	remotes, err := windowsWebDAVRemotes(req.URL)
	if err != nil {
		return false, err
	}
	uses, err := windowsAllDriveUses(ctx)
	if err != nil {
		return false, err
	}
	_, ok, err := findUsableWindowsWebDAVMapping(ctx, uses, remotes, drive)
	return ok, err
}

func findUsableWindowsWebDAVMapping(ctx context.Context, uses []windowsDriveUse, remotes []string, drive string) (windowsDriveUse, bool, error) {
	for _, use := range uses {
		if !windowsDriveMatches(use.Local, drive) {
			continue
		}
		usable, err := windowsMappingUsable(ctx, use, remotes)
		if err != nil {
			return windowsDriveUse{}, false, err
		}
		if usable {
			return use, true, nil
		}
	}
	return windowsDriveUse{}, false, nil
}

func windowsMappingUsable(ctx context.Context, use windowsDriveUse, remotes []string) (bool, error) {
	if !use.Mapped || strings.TrimSpace(use.Local) == "" {
		return false, nil
	}
	if !windowsUseStatusOK(use.Status) {
		return false, nil
	}
	if !windowsRemoteMatches(use.Remote, remotes) {
		return false, nil
	}
	return windowsDrivePathExists(ctx, use.Local)
}

func windowsDriveMatches(local, drive string) bool {
	drive = strings.ToUpper(strings.TrimSpace(drive))
	if drive == "" || drive == "*" {
		return true
	}
	return strings.EqualFold(strings.TrimSpace(local), drive)
}

func windowsUseStatusOK(status string) bool {
	status = strings.TrimSpace(status)
	return status == "" || strings.EqualFold(status, "OK")
}

func windowsDrivePathExists(ctx context.Context, drive string) (bool, error) {
	if strings.TrimSpace(drive) == "" || drive == "*" {
		return false, nil
	}
	if _, err := windowsRunMountCommand(ctx, "cmd", "/C", "if exist "+drive+`\NUL`+" (exit 0) else (exit 1)"); err == nil {
		return true, nil
	} else if ctx.Err() != nil {
		return false, ctx.Err()
	}
	return false, nil
}

func windowsRemoteMatches(remote string, remotes []string) bool {
	if strings.TrimSpace(remote) == "" {
		return false
	}
	text := normalizeWindowsRemote(remote)
	for _, remote := range remotes {
		if text == normalizeWindowsRemote(remote) {
			return true
		}
	}
	return false
}

func normalizeWindowsRemote(remote string) string {
	remote = strings.TrimSpace(remote)
	remote = strings.TrimRight(remote, `\/`)
	return strings.ToLower(remote)
}

func windowsRemoteLooksLikeCNCWebDAV(remote string) bool {
	remote = strings.ToLower(remote)
	for _, marker := range []string{
		"127.0.0.1:8421",
		"localhost:8421",
		"127.0.0.1@8421",
		"localhost@8421",
		"127.0.0.1:8430",
		"localhost:8430",
		"127.0.0.1@8430",
		"localhost@8430",
	} {
		if strings.Contains(remote, marker) {
			return true
		}
	}
	return false
}

func windowsWebDAVRemotes(raw string) ([]string, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return nil, err
	}
	host := u.Hostname()
	if host == "" {
		return nil, fmt.Errorf("webdav URL has no host: %q", raw)
	}
	remotes := windowsWebDAVUNCs(u)
	remotes = append(remotes, u.String(), strings.TrimRight(u.String(), "/"))
	if host == "127.0.0.1" {
		localhost := *u
		localhost.Host = net.JoinHostPort("localhost", u.Port())
		if u.Port() == "" {
			localhost.Host = "localhost"
		}
		remotes = append(remotes, localhost.String(), strings.TrimRight(localhost.String(), "/"))
	}
	return dedupeStrings(remotes), nil
}

func windowsWebDAVUNCs(u *url.URL) []string {
	hosts := []string{u.Hostname()}
	if u.Hostname() == "127.0.0.1" {
		hosts = append([]string{"localhost"}, hosts...)
	}
	rawPath := strings.Trim(u.EscapedPath(), "/")
	davPath := "DavWWWRoot"
	plainPath := ""
	if rawPath != "" {
		davPath = "DavWWWRoot\\" + strings.ReplaceAll(rawPath, "/", "\\")
		plainPath = strings.ReplaceAll(rawPath, "/", "\\")
	}
	var paths []string
	if plainPath == "" {
		paths = []string{davPath}
	} else {
		paths = []string{davPath, plainPath}
	}
	var out []string
	for _, host := range hosts {
		if ip := net.ParseIP(host); ip != nil && ip.To4() == nil {
			host = "[" + host + "]"
		}
		authority := host
		if port := u.Port(); port != "" {
			authority += "@" + port
		}
		for _, path := range paths {
			out = append(out, `\\`+authority+`\`+path)
		}
	}
	return out
}

func dedupeStrings(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range in {
		if strings.TrimSpace(s) == "" || seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	return out
}

func emptyText(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return "<empty>"
	}
	return s
}

func windowsRunMountCommand(ctx context.Context, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	configureBackgroundCommand(cmd)
	token, err := windowsShellUserToken()
	if err != nil {
		return "", err
	}
	if token == 0 {
		var out bytes.Buffer
		cmd.Stdout = &out
		cmd.Stderr = &out
		if err := cmd.Run(); err != nil {
			return out.String(), err
		}
		return out.String(), nil
	}
	defer token.Close()
	return windowsRunWithToken(ctx, token, name, args...)
}

type windowsExitError struct {
	code uint32
}

func (e windowsExitError) Error() string {
	return fmt.Sprintf("exit status %d", e.code)
}

type windowsWaitResult struct {
	code uint32
	err  error
}

func windowsRunWithToken(ctx context.Context, token windows.Token, name string, args ...string) (string, error) {
	exe, err := exec.LookPath(name)
	if err != nil {
		return "", err
	}
	appName, err := windows.UTF16PtrFromString(exe)
	if err != nil {
		return "", err
	}
	commandLine, err := windows.UTF16PtrFromString(windows.ComposeCommandLine(append([]string{exe}, args...)))
	if err != nil {
		return "", err
	}

	var readPipe, writePipe windows.Handle
	sa := windows.SecurityAttributes{Length: uint32(unsafe.Sizeof(windows.SecurityAttributes{})), InheritHandle: 1}
	if err := windows.CreatePipe(&readPipe, &writePipe, &sa, 0); err != nil {
		return "", err
	}
	if err := windows.SetHandleInformation(readPipe, windows.HANDLE_FLAG_INHERIT, 0); err != nil {
		_ = windows.CloseHandle(readPipe)
		_ = windows.CloseHandle(writePipe)
		return "", err
	}

	readFile := os.NewFile(uintptr(readPipe), "webdav-mount-output")
	if readFile == nil {
		_ = windows.CloseHandle(readPipe)
		_ = windows.CloseHandle(writePipe)
		return "", errors.New("create WebDAV mount output pipe")
	}
	outputDone := make(chan []byte, 1)
	go func() {
		b, _ := io.ReadAll(readFile)
		_ = readFile.Close()
		outputDone <- b
	}()

	si := windows.StartupInfo{
		Cb:         uint32(unsafe.Sizeof(windows.StartupInfo{})),
		Flags:      windows.STARTF_USESTDHANDLES | windows.STARTF_USESHOWWINDOW,
		ShowWindow: windows.SW_HIDE,
		StdOutput:  writePipe,
		StdErr:     writePipe,
	}
	var pi windows.ProcessInformation
	err = windowsCreateProcessWithToken(token, appName, commandLine, windows.CREATE_NO_WINDOW|windows.CREATE_UNICODE_ENVIRONMENT, nil, nil, &si, &pi)
	_ = windows.CloseHandle(writePipe)
	if err != nil {
		out := <-outputDone
		return string(out), err
	}
	defer windows.CloseHandle(pi.Process)
	defer windows.CloseHandle(pi.Thread)

	waitDone := make(chan windowsWaitResult, 1)
	go func() {
		if _, err := windows.WaitForSingleObject(pi.Process, windows.INFINITE); err != nil {
			waitDone <- windowsWaitResult{err: err}
			return
		}
		var code uint32
		if err := windows.GetExitCodeProcess(pi.Process, &code); err != nil {
			waitDone <- windowsWaitResult{err: err}
			return
		}
		waitDone <- windowsWaitResult{code: code}
	}()

	var result windowsWaitResult
	select {
	case result = <-waitDone:
	case <-ctx.Done():
		_ = windows.TerminateProcess(pi.Process, 1)
		result = <-waitDone
		out := <-outputDone
		return string(out), ctx.Err()
	}
	out := <-outputDone
	if result.err != nil {
		return string(out), result.err
	}
	if result.code != 0 {
		return string(out), windowsExitError{code: result.code}
	}
	return string(out), nil
}

func windowsCreateProcessWithToken(token windows.Token, appName, commandLine *uint16, creationFlags uint32, env, cwd *uint16, startupInfo *windows.StartupInfo, processInfo *windows.ProcessInformation) error {
	const logonWithProfile = 0x00000001
	r1, _, e1 := procCreateProcessWithTokenW.Call(
		uintptr(token),
		uintptr(logonWithProfile),
		uintptr(unsafe.Pointer(appName)),
		uintptr(unsafe.Pointer(commandLine)),
		uintptr(creationFlags),
		uintptr(unsafe.Pointer(env)),
		uintptr(unsafe.Pointer(cwd)),
		uintptr(unsafe.Pointer(startupInfo)),
		uintptr(unsafe.Pointer(processInfo)),
	)
	if r1 != 0 {
		return nil
	}
	if errno, ok := e1.(syscall.Errno); ok && errno == 0 {
		return syscall.EINVAL
	}
	return e1
}

func windowsShellUserToken() (windows.Token, error) {
	current := windows.GetCurrentProcessToken()
	if !current.IsElevated() {
		return 0, nil
	}
	currentUser, err := current.GetTokenUser()
	if err != nil {
		return 0, fmt.Errorf("read elevated tray user token: %w", err)
	}
	activeSession := windows.WTSGetActiveConsoleSessionId()
	if activeSession != 0xffffffff {
		if token, err := windowsExplorerTokenForUser(currentUser.User.Sid, &activeSession); err == nil {
			return token, nil
		}
	}
	token, err := windowsExplorerTokenForUser(currentUser.User.Sid, nil)
	if err != nil {
		return 0, fmt.Errorf("tray process is elevated; cannot run WebDAV mount in the desktop Explorer context: %w", err)
	}
	return token, nil
}

func windowsExplorerTokenForUser(userSID *windows.SID, sessionID *uint32) (windows.Token, error) {
	snapshot, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return 0, fmt.Errorf("enumerate processes: %w", err)
	}
	defer windows.CloseHandle(snapshot)

	var lastErr error
	var entry windows.ProcessEntry32
	entry.Size = uint32(unsafe.Sizeof(entry))
	for err := windows.Process32First(snapshot, &entry); err == nil; err = windows.Process32Next(snapshot, &entry) {
		if !strings.EqualFold(windows.UTF16ToString(entry.ExeFile[:]), "explorer.exe") {
			continue
		}
		if sessionID != nil {
			var processSession uint32
			if err := windows.ProcessIdToSessionId(entry.ProcessID, &processSession); err != nil || processSession != *sessionID {
				continue
			}
		}
		token, err := windowsOpenProcessToken(entry.ProcessID)
		if err != nil {
			lastErr = err
			continue
		}
		tokenUser, err := token.GetTokenUser()
		if err != nil {
			_ = token.Close()
			lastErr = err
			continue
		}
		if !windows.EqualSid(tokenUser.User.Sid, userSID) {
			_ = token.Close()
			continue
		}
		launchToken, err := windowsDuplicateLaunchToken(token)
		_ = token.Close()
		if err != nil {
			lastErr = err
			continue
		}
		return launchToken, nil
	}
	if err != nil && !errors.Is(err, windows.ERROR_NO_MORE_FILES) {
		return 0, fmt.Errorf("enumerate explorer.exe processes: %w", err)
	}
	if lastErr != nil {
		return 0, lastErr
	}
	return 0, errors.New("no explorer.exe process for the tray user was found")
}

func windowsOpenProcessToken(pid uint32) (windows.Token, error) {
	process, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, pid)
	if err != nil {
		return 0, fmt.Errorf("open explorer.exe pid %d: %w", pid, err)
	}
	defer windows.CloseHandle(process)
	var token windows.Token
	access := uint32(windows.TOKEN_ASSIGN_PRIMARY | windows.TOKEN_DUPLICATE | windows.TOKEN_QUERY | windows.TOKEN_ADJUST_DEFAULT | windows.TOKEN_ADJUST_SESSIONID)
	if err := windows.OpenProcessToken(process, access, &token); err != nil {
		return 0, fmt.Errorf("open explorer.exe pid %d token: %w", pid, err)
	}
	return token, nil
}

func windowsDuplicateLaunchToken(token windows.Token) (windows.Token, error) {
	access := uint32(windows.TOKEN_ASSIGN_PRIMARY | windows.TOKEN_DUPLICATE | windows.TOKEN_QUERY | windows.TOKEN_ADJUST_DEFAULT | windows.TOKEN_ADJUST_SESSIONID)
	var out windows.Token
	err := windows.DuplicateTokenEx(token, access, nil, windows.SecurityImpersonation, windows.TokenPrimary, &out)
	if err != nil {
		return 0, fmt.Errorf("duplicate explorer.exe token for WebDAV mount process: %w", err)
	}
	return out, nil
}
