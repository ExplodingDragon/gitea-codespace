// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package provisioner

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestBuiltinStartHTTPDoesNotUseGitSSH(t *testing.T) {
	t.Parallel()

	workspaceRoot := filepath.Join(t.TempDir(), "workspaces")
	workspace := filepath.Join(workspaceRoot, "repo")
	writeTestWorkspace(t, workspace, "https://gitea.example.com/owner/repo.git", "user-head\n")
	result := runBuiltinScriptForTest(t, builtinStartScript, map[string]string{
		"CODESPACE_WORKSPACES_DIR":  workspaceRoot,
		"CODESPACE_WORKSPACE_DIR":   workspace,
		"CODESPACE_REPO_NAME":       "repo",
		"GITEA_GIT_PROTOCOL":        "http",
		"GITEA_REPO_CLONE_HTTP_URL": "https://gitea.example.com/owner/repo.git",
		"GITEA_REPO_CLONE_SSH_URL":  "",
		"GITEA_COMMIT_SHA":          "",
		"GITEA_START_REF":           "",
	})

	if err := validateScriptResult(result.resultContent, "start-environment"); err != nil {
		t.Fatalf("validate result: %v", err)
	}
	if strings.Contains(result.commandLog, "curl ") ||
		strings.Contains(result.commandLog, "ssh-keygen ") ||
		strings.Contains(result.commandLog, "known_hosts") {
		t.Fatalf("HTTP start used Git SSH path:\n%s", result.commandLog)
	}
	if !strings.Contains(result.commandLog, "credential.helper") {
		t.Fatalf("HTTP start did not configure credential helper:\n%s", result.commandLog)
	}
	if !strings.Contains(result.envContent, "CODESPACE_WORKSPACE_DIR=") {
		t.Fatalf("shared env = %q", result.envContent)
	}
	if strings.Contains(result.commandLog, " clone ") ||
		strings.Contains(result.commandLog, " fetch ") ||
		strings.Contains(result.commandLog, " checkout ") {
		t.Fatalf("start changed workspace:\n%s", result.commandLog)
	}
}

func TestBuiltinStartUsesExistingRemote(t *testing.T) {
	t.Parallel()

	workspaceRoot := filepath.Join(t.TempDir(), "workspaces")
	workspace := filepath.Join(workspaceRoot, "repo")
	writeTestWorkspace(t, workspace, "https://gitea.example.com/owner/repo.git", "user-head\n")
	result := runBuiltinScriptForTest(t, builtinStartScript, map[string]string{
		"CODESPACE_WORKSPACES_DIR":  workspaceRoot,
		"CODESPACE_WORKSPACE_DIR":   workspace,
		"CODESPACE_REPO_NAME":       "repo",
		"GITEA_GIT_PROTOCOL":        "ssh",
		"GITEA_REPO_CLONE_HTTP_URL": "https://gitea.example.com/owner/repo.git",
		"GITEA_REPO_CLONE_SSH_URL":  "",
		"GITEA_COMMIT_SHA":          "",
		"GITEA_START_REF":           "",
	})

	if err := validateScriptResult(result.resultContent, "start-environment"); err != nil {
		t.Fatalf("validate result: %v", err)
	}
	if strings.Contains(result.commandLog, "curl ") ||
		strings.Contains(result.commandLog, "ssh-keygen ") ||
		strings.Contains(result.commandLog, "known_hosts") ||
		strings.Contains(result.commandLog, " clone ") {
		t.Fatalf("start did not use existing HTTP remote:\n%s", result.commandLog)
	}
}

func TestBuiltinStartKeepsWorkspaceHEAD(t *testing.T) {
	t.Parallel()

	workspaceRoot := filepath.Join(t.TempDir(), "workspaces")
	workspace := filepath.Join(workspaceRoot, "repo")
	if err := os.MkdirAll(filepath.Join(workspace, ".git"), 0o755); err != nil {
		t.Fatalf("create workspace: %v", err)
	}
	if err := os.WriteFile(filepath.Join(workspace, ".git", "origin"), []byte("https://gitea.example.com/owner/repo.git\n"), 0o644); err != nil {
		t.Fatalf("write origin: %v", err)
	}
	if err := os.WriteFile(filepath.Join(workspace, ".git", "HEAD"), []byte("user-head\n"), 0o644); err != nil {
		t.Fatalf("write head: %v", err)
	}

	result := runBuiltinScriptForTest(t, builtinStartScript, map[string]string{
		"CODESPACE_WORKSPACES_DIR": workspaceRoot,
		"CODESPACE_WORKSPACE_DIR":  workspace,
	})

	if err := validateScriptResult(result.resultContent, "start-environment"); err != nil {
		t.Fatalf("validate result: %v", err)
	}
	if strings.Contains(result.commandLog, " fetch ") ||
		strings.Contains(result.commandLog, " checkout ") {
		t.Fatalf("start changed repository HEAD path:\n%s", result.commandLog)
	}
	head, err := os.ReadFile(filepath.Join(workspace, ".git", "HEAD"))
	if err != nil {
		t.Fatalf("read head: %v", err)
	}
	if string(head) != "user-head\n" {
		t.Fatalf("head = %q", string(head))
	}
}

func TestDevcontainerExampleStartStopAndPortForward(t *testing.T) {
	t.Parallel()

	workspaceRoot := filepath.Join(t.TempDir(), "workspaces")
	workspace := filepath.Join(workspaceRoot, "repo")
	writeTestWorkspace(t, workspace, "https://gitea.example.com/owner/repo.git", "user-head\n")
	start := runDevcontainerExampleScriptForTest(t, "start.sh", map[string]string{
		"CODESPACE_WORKSPACES_DIR":  workspaceRoot,
		"CODESPACE_WORKSPACE_DIR":   workspace,
		"CODESPACE_REPO_NAME":       "repo",
		"GITEA_GIT_PROTOCOL":        "http",
		"GITEA_REPO_CLONE_HTTP_URL": "https://gitea.example.com/owner/repo.git",
		"GITEA_COMMIT_SHA":          "0123456789abcdef0123456789abcdef01234567",
		"GITEA_START_REF":           "refs/heads/main",
	})
	if err := validateScriptResult(start.resultContent, "start-environment"); err != nil {
		t.Fatalf("validate start result: %v", err)
	}
	startEnv := parseSharedEnvForTest(start.envContent)
	if startEnv["CODESPACE_WORKSPACE_DIR"] != workspace {
		t.Fatalf("start shared env = %#v", startEnv)
	}
	if startEnv["DEVCONTAINER_EXAMPLE_CONTAINER_ID"] != "container-1" ||
		startEnv["CODESPACE_INTERNAL_SSH_PORT"] != "" ||
		startEnv["CODESPACE_INTERNAL_SSH_USER"] != "" ||
		startEnv["CODESPACE_INTERNAL_SSH_HOST_KEY_FINGERPRINT"] != "" {
		t.Fatalf("start shared env = %#v", startEnv)
	}
	if !strings.Contains(start.commandLog, "devcontainer up") ||
		strings.Contains(start.commandLog, "git clone") ||
		strings.Contains(start.commandLog, "git checkout") ||
		strings.Contains(start.commandLog, "socat TCP-LISTEN:2222") ||
		strings.Contains(start.commandLog, "sshd") {
		t.Fatalf("start did not use unified devcontainer path:\n%s", start.commandLog)
	}

	restart := runDevcontainerExampleScriptForTest(t, "start.sh", map[string]string{
		"CODESPACE_WORKSPACE_DIR":           workspace,
		"DEVCONTAINER_EXAMPLE_CONTAINER_ID": "container-1",
	})
	if err := validateScriptResult(restart.resultContent, "start-environment"); err != nil {
		t.Fatalf("validate restart result: %v", err)
	}
	if strings.Contains(restart.commandLog, "devcontainer up") ||
		strings.Contains(restart.commandLog, "git checkout") {
		t.Fatalf("restart rebuilt container or changed HEAD:\n%s", restart.commandLog)
	}
	if !strings.Contains(restart.commandLog, "docker start container-1") {
		t.Fatalf("restart did not start existing container:\n%s", restart.commandLog)
	}

	stop := runDevcontainerExampleScriptForTest(t, "stop.sh", map[string]string{
		"CODESPACE_WORKSPACE_DIR":           workspace,
		"DEVCONTAINER_EXAMPLE_CONTAINER_ID": "container-1",
	})
	if err := validateScriptResult(stop.resultContent, "stop-environment"); err != nil {
		t.Fatalf("validate stop result: %v", err)
	}
	if !strings.Contains(stop.commandLog, "docker stop container-1") {
		t.Fatalf("stop did not stop container:\n%s", stop.commandLog)
	}
}

type builtinScriptTestResult struct {
	commandLog    string
	envContent    string
	resultContent string
	workspaceRoot string
}

func runBuiltinScriptForTest(t *testing.T, script string, environment map[string]string) builtinScriptTestResult {
	t.Helper()

	dir := t.TempDir()
	binDir := filepath.Join(dir, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatalf("create fake bin: %v", err)
	}
	commandLog := filepath.Join(dir, "commands.log")
	installBuiltinScriptFakes(t, binDir)

	scriptPath := filepath.Join(dir, "script.sh")
	if err := os.WriteFile(scriptPath, []byte(script), 0o700); err != nil {
		t.Fatalf("write script: %v", err)
	}
	resultPath := filepath.Join(dir, "result.json")
	envPath := filepath.Join(dir, "env")
	if err := os.WriteFile(envPath, nil, 0o600); err != nil {
		t.Fatalf("write env: %v", err)
	}
	seedDir := filepath.Join(dir, "runtime", "seed")
	writeTestRuntimeSeed(t, seedDir)

	workspaceRoot := environment["CODESPACE_WORKSPACES_DIR"]
	if workspaceRoot == "" {
		workspaceRoot = filepath.Join(dir, "workspaces")
		environment["CODESPACE_WORKSPACES_DIR"] = workspaceRoot
	}
	environment["CODESPACE_RESULT"] = resultPath
	environment["CODESPACE_ENV"] = envPath
	environment["CODESPACE_RUNTIME_DIR"] = filepath.Join(dir, "runtime")
	environment["CODESPACE_RUNTIME_SEED_DIR"] = seedDir
	environment["GITEA_TEST_LOG"] = commandLog

	command := exec.Command("sh", scriptPath)
	command.Env = append(os.Environ(), "PATH="+binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	for name, value := range environment {
		command.Env = append(command.Env, name+"="+value)
	}
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("run builtin script: %v\n%s", err, string(output))
	}

	return builtinScriptTestResult{
		commandLog:    readTestFile(t, commandLog),
		envContent:    readTestFile(t, envPath),
		resultContent: readTestFile(t, resultPath),
		workspaceRoot: workspaceRoot,
	}
}

func runDevcontainerExampleScriptForTest(t *testing.T, name string, environment map[string]string) builtinScriptTestResult {
	t.Helper()

	dir := t.TempDir()
	binDir := filepath.Join(dir, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatalf("create fake bin: %v", err)
	}
	commandLog := filepath.Join(dir, "commands.log")
	installDevcontainerScriptFakes(t, binDir)

	resultPath := filepath.Join(dir, "result.json")
	envPath := filepath.Join(dir, "env")
	if err := os.WriteFile(envPath, nil, 0o600); err != nil {
		t.Fatalf("write env: %v", err)
	}
	seedDir := filepath.Join(dir, "runtime", "seed")
	writeTestRuntimeSeed(t, seedDir)
	environment["CODESPACE_RESULT"] = resultPath
	environment["CODESPACE_ENV"] = envPath
	environment["CODESPACE_RUNTIME_DIR"] = filepath.Join(dir, "runtime")
	environment["CODESPACE_RUNTIME_SEED_DIR"] = seedDir
	environment["GITEA_TEST_LOG"] = commandLog
	environment["GITEA_TEST_CONTAINER_ID_FILE"] = filepath.Join(dir, "container-id")
	environment["DEVCONTAINER_EXAMPLE_AUTHORIZED_KEY_FILE"] = filepath.Join(dir, "authorized_keys")
	environment["DEVCONTAINER_EXAMPLE_RUN_DIR"] = filepath.Join(dir, "run")
	environment["DEVCONTAINER_EXAMPLE_LOG_DIR"] = filepath.Join(dir, "log")

	scriptPath := filepath.Join("..", "..", "examples", "devcontainer", name)
	command := exec.Command("sh", scriptPath)
	command.Env = append(os.Environ(), "PATH="+binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	for key, value := range environment {
		command.Env = append(command.Env, key+"="+value)
	}
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("run devcontainer example %s: %v\n%s", name, err, string(output))
	}

	return builtinScriptTestResult{
		commandLog:    readTestFile(t, commandLog),
		envContent:    readTestFile(t, envPath),
		resultContent: readTestFile(t, resultPath),
		workspaceRoot: environment["CODESPACE_WORKSPACES_DIR"],
	}
}

func installBuiltinScriptFakes(t *testing.T, binDir string) {
	t.Helper()

	writeExecutableForTest(t, filepath.Join(binDir, "id"), `#!/bin/bash
set -eu
case "${1:-}" in
  -u|-g) printf '1000\n' ;;
  *) exit 0 ;;
esac
`)
	writeExecutableForTest(t, filepath.Join(binDir, "install"), `#!/bin/bash
set -eu
directory=0
args=""
while [ "$#" -gt 0 ]; do
  case "$1" in
    -d) directory=1; shift ;;
    -m|-o|-g) shift 2 ;;
    -*) shift ;;
    *) args="${args}
$1"; shift ;;
  esac
done
set -- $args
if [ "$directory" = "1" ]; then
  mkdir -p "$@"
  exit 0
fi
while [ "$#" -gt 2 ]; do
  shift
done
src="$1"
dst="$2"
mkdir -p "$(dirname "$dst")"
cp "$src" "$dst"
`)
	writeExecutableForTest(t, filepath.Join(binDir, "sudo"), `#!/bin/bash
set -eu
if [ "${1:-}" = "-u" ]; then
  shift 2
fi
if [ "${1:-}" = "env" ]; then
  shift
  while [ "$#" -gt 0 ]; do
    case "$1" in
      *=*) export "$1"; shift ;;
      *) break ;;
    esac
  done
fi
exec "$@"
`)
	writeExecutableForTest(t, filepath.Join(binDir, "chown"), "#!/bin/bash\nexit 0\n")
	writeExecutableForTest(t, filepath.Join(binDir, "chmod"), "#!/bin/bash\nexit 0\n")
	writeExecutableForTest(t, filepath.Join(binDir, "curl"), `#!/bin/bash
set -eu
printf 'curl %s\n' "$*" >> "$GITEA_TEST_LOG"
printf '{"known_hosts_lines":["gitea.example.com ssh-ed25519 AAAA"]}\n'
`)
	writeExecutableForTest(t, filepath.Join(binDir, "ssh-keygen"), `#!/bin/bash
set -eu
printf 'ssh-keygen %s\n' "$*" >> "$GITEA_TEST_LOG"
while [ "$#" -gt 0 ]; do
  if [ "$1" = "-f" ]; then
    shift
    mkdir -p "$(dirname "$1")"
    printf 'private-key\n' > "$1"
    printf 'public-key\n' > "$1.pub"
    exit 0
  fi
  shift
done
`)
	writeExecutableForTest(t, filepath.Join(binDir, "git"), `#!/bin/bash
set -eu
printf 'git %s\n' "$*" >> "$GITEA_TEST_LOG"
workdir=""
if [ "${1:-}" = "-C" ]; then
  workdir="$2"
  shift 2
fi
command="$1"
shift || true
case "$command" in
  clone)
    repo="$1"
    dest="$2"
    mkdir -p "$dest/.git" "$dest/.devcontainer"
    printf '%s\n' "$repo" > "$dest/.git/origin"
    printf 'cloned-head\n' > "$dest/.git/HEAD"
    printf '{"image":"debian:12"}\n' > "$dest/.devcontainer/devcontainer.json"
    ;;
  config)
    ;;
  remote)
    if [ "${1:-}" = "get-url" ]; then
      cat "$workdir/.git/origin"
    fi
    ;;
  fetch)
    ;;
  checkout)
    printf '%s\n' "$2" > "$workdir/.git/HEAD"
    ;;
  rev-parse)
    cat "$workdir/.git/HEAD"
    ;;
esac
`)
}

func installDevcontainerScriptFakes(t *testing.T, binDir string) {
	t.Helper()

	installBuiltinScriptFakes(t, binDir)
	writeExecutableForTest(t, filepath.Join(binDir, "devcontainer"), `#!/bin/bash
set -eu
printf 'devcontainer %s\n' "$*" >> "$GITEA_TEST_LOG"
printf 'container-1\n' > "$GITEA_TEST_CONTAINER_ID_FILE"
`)
	writeExecutableForTest(t, filepath.Join(binDir, "docker"), `#!/bin/bash
set -eu
printf 'docker %s\n' "$*" >> "$GITEA_TEST_LOG"
case "${1:-}" in
  ps)
    if [ -f "$GITEA_TEST_CONTAINER_ID_FILE" ]; then
      cat "$GITEA_TEST_CONTAINER_ID_FILE"
    fi
    ;;
  start)
    ;;
  exec)
    case "$*" in
      *"ssh-keygen -lf"*)
        printf '256 SHA256:devcontainerhostkey /etc/ssh/ssh_host_ed25519_key.pub (ED25519)\n'
        ;;
    esac
    ;;
  inspect)
    printf '172.18.0.2\n'
    ;;
esac
`)
	writeExecutableForTest(t, filepath.Join(binDir, "nohup"), `#!/bin/bash
set -eu
printf 'nohup %s\n' "$*" >> "$GITEA_TEST_LOG"
`)
}

func parseSharedEnvForTest(content string) map[string]string {
	environment := map[string]string{}
	for _, line := range strings.Split(content, "\n") {
		if line == "" {
			continue
		}
		name, value, ok := strings.Cut(line, "=")
		if ok {
			environment[name] = value
		}
	}
	return environment
}

func writeExecutableForTest(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o700); err != nil {
		t.Fatalf("write executable %s: %v", path, err)
	}
}

func writeTestWorkspace(t *testing.T, workspace, origin, head string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(workspace, ".git"), 0o755); err != nil {
		t.Fatalf("create workspace: %v", err)
	}
	if err := os.WriteFile(filepath.Join(workspace, ".git", "origin"), []byte(origin+"\n"), 0o644); err != nil {
		t.Fatalf("write origin: %v", err)
	}
	if err := os.WriteFile(filepath.Join(workspace, ".git", "HEAD"), []byte(head), 0o644); err != nil {
		t.Fatalf("write head: %v", err)
	}
}

func writeTestRuntimeSeed(t *testing.T, seedDir string) {
	t.Helper()
	if err := os.MkdirAll(seedDir, 0o700); err != nil {
		t.Fatalf("create seed dir: %v", err)
	}
	for name, content := range map[string]string{
		"gitea-token":    "token\n",
		"id_ed25519":     "private-key\n",
		"id_ed25519.pub": "public-key\n",
		"known_hosts":    "",
	} {
		if err := os.WriteFile(filepath.Join(seedDir, name), []byte(content), 0o600); err != nil {
			t.Fatalf("write seed %s: %v", name, err)
		}
	}
}

func readTestFile(t *testing.T, path string) string {
	t.Helper()
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(content)
}
