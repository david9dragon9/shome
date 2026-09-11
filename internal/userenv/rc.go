package userenv

import (
	"os"
	"path/filepath"
)

// rcTemplate is the startup file every interactive session sources.
//
// It lives in shome's managed directory, not in the account's home, for two
// reasons: the account cannot edit it (so the prompt and the tool paths are
// dependable), and upgrading shome upgrades it for everybody at once. The
// account's own ~/.bashrc is sourced at the end, so anyone who wants their
// own aliases and settings still gets them -- and if they break something,
// what they broke is their file, not shome's.
const rcTemplate = `# shome interactive session (generated -- do not edit)
#
# Sourced for every login shell on this machine. Your own ~/.bashrc is read
# at the end of this file, so put your settings there.

# username@host.cluster, so it is always clear which machine you are on and
# which cluster it belongs to. Both come from the session's environment, so
# renaming the cluster changes the prompt without anyone reconnecting.
__shome_where() {
  local host="${SHOME_HOST:-unknown}" cluster="${SHOME_CLUSTER:-shome}"
  printf '%s.%s' "$host" "$cluster"
}
# ~ for the home directory, since the real path is long and uninformative.
# bash's own \w does this; a hand-rolled ${PWD/#$HOME/~} silently stops
# matching once quoted, which is how the prompt ended up showing the whole
# path. Only the sh fallback needs the function.
__shome_cwd() {
  case "$PWD" in
    "$HOME") printf '~' ;;
    "$HOME"/*) printf '~%s' "${PWD#$HOME}" ;;
    *) printf '%s' "$PWD" ;;
  esac
}
if [ -n "$BASH_VERSION" ]; then
  PS1='\[\033[1;32m\]${SHOME_USER:-\u}@$(__shome_where)\[\033[0m\]:\[\033[1;34m\]\w\[\033[0m\]$ '
else
  PS1='${SHOME_USER}@'"$(__shome_where)"':$(__shome_cwd)$ '
fi
export PS1

# The environment for this machine's kind is activated automatically, so
# "make an environment once, then just use python" works the way people
# expect. $SHOME_VENV is per os-arch: one home directory is used by both a
# Linux container and, for GPU work on a Mac, the host itself, and a
# virtualenv built for one cannot run in the other.
if [ -n "$SHOME_VENV" ] && [ -f "$SHOME_VENV/bin/activate" ]; then
  . "$SHOME_VENV/bin/activate"
fi

shome-help() {
  cat <<'ENDHELP'
This is a shome session. Your home directory is yours and persists; it is
also the only place you can write.

  uv venv $SHOME_VENV         create your environment (activated from now on)
  uv pip install numpy        install into it
  uv run script.py            run with dependencies resolved
  uv tool install ruff        install a command-line tool onto your PATH
  uv python install 3.13      install an interpreter

Your environment is per machine kind ($SHOME_SLOT here) and lives in your
home directory, so jobs on any machine of the same kind find it -- and a
GPU job on a Mac, which runs outside the container, gets its own.

Your files, across every machine in the cluster:

  shome fs du                 how much you are using, and your limit
  shome fs ls                 everything, everywhere
  shome fs sync HERE OTHER:   copy this machine's files to another

Running work on the cluster:

  sbatch job.sh               submit a batch job
  srun COMMAND                run something now and watch it
  srun --pty bash             an interactive shell on a compute node
  squeue                      what is queued and running
  sshare                      your fair-share standing
  squota                      what you are using, and what you are allowed

ENDHELP
  # Where you are decides the last paragraph. The same file is read on the
  # login node and inside an interactive job, and telling somebody already
  # on a compute node to use srun is advice that cannot be followed.
  if [ -n "$SHOME_JOB_ID" ]; then
    cat <<ENDJOB

You are inside job $SHOME_JOB_ID on ${SHOME_HOST}, holding its resources until
you exit. Anything you install goes in your home directory and is still
there next time.
ENDJOB
  else
    cat <<'ENDLOGIN'

This machine is a login node: it is deliberately small. Anything that needs
real CPU, memory or a GPU belongs in srun or sbatch.
ENDLOGIN
  fi
}

# Shown once per session. A session that says nothing leaves people guessing
# what they are allowed to do, which was the original complaint.
if [ -z "$SHOME_QUIET" ]; then
  # The job id when there is one: a shell on a compute node looks exactly
  # like one on the login node otherwise, and which it is decides what the
  # machine's load means and what exiting costs.
  __shome_job=""
  [ -n "$SHOME_JOB_ID" ] && __shome_job=" (job $SHOME_JOB_ID)"
  printf '%s\n' "shome: ${SHOME_USER} on ${SHOME_HOST}.${SHOME_CLUSTER}${__shome_job}"
  unset __shome_job
  if command -v uv >/dev/null 2>&1; then
    printf '  %s, python via uv. ' "$(uv --version 2>/dev/null)"
  fi
  printf 'Type shome-help for what is here.\n'
fi

[ -f "$HOME/.bashrc" ] && . "$HOME/.bashrc"
`

// writeRC installs the session startup file.
func writeRC(root string) error {
	if err := os.MkdirAll(Dir(root), 0o755); err != nil {
		return err
	}
	tmp := RCPath(root) + ".tmp"
	if err := os.WriteFile(tmp, []byte(rcTemplate), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, RCPath(root))
}

// EnsureRC writes the startup file if it is missing or out of date.
//
// Compared by content rather than by a version stamp, so an upgrade that
// changes the template takes effect and one that does not leaves the file
// alone.
func EnsureRC(root string) error {
	if b, err := os.ReadFile(RCPath(root)); err == nil && string(b) == rcTemplate {
		return nil
	}
	return writeRC(root)
}

// rcName is the startup file's name wherever it is placed.
const rcName = "shome-rc.sh"

// ShellArgvAt is the command an interactive session runs, given the tool
// directory as the session will see it.
//
// Used where the session's filesystem is not the host's -- a container, or a
// mount namespace -- so the startup file has to be named by its path inside.
// bash is assumed rather than probed for the same reason: what exists on the
// host says nothing about what exists in there.
func ShellArgvAt(toolsIn string) []string {
	return []string{"/bin/bash", "--rcfile", toolsIn + "/" + rcName, "-i"}
}

// InteractiveShellArgv gives a bare interactive shell the cluster's startup
// file, and leaves anything else alone.
//
// For `srun --pty bash`, which is a shell on a compute node and should look
// like one: same prompt, same helpers, same virtualenv activated. Without
// it bash reads whatever startup file it finds, and inside the session
// image that is Debian's own -- which sets PS1 unconditionally and produced
// the `root@<container id>` prompt, naming a user the job is not and a
// machine nobody can find.
//
// Only a bare shell is rewritten. `srun --pty python` or `bash -c ...` is a
// command the submitter chose, and adding arguments to it would be shome
// deciding it knows better.
//
// argv[0] is kept as written rather than replaced with /bin/bash: it is
// already what the job would have run, so a machine whose bash is
// somewhere else keeps working.
func InteractiveShellArgv(argv []string, toolsIn string) []string {
	if toolsIn == "" || len(argv) != 1 {
		return argv
	}
	switch argv[0] {
	case "bash", "/bin/bash", "/usr/bin/bash":
		return []string{argv[0], "--rcfile", toolsIn + "/" + rcName, "-i"}
	}
	return argv
}

// ShellArgv is the command an interactive session runs.
//
// bash with an explicit --rcfile when it is available, because that is the
// only way to guarantee the prompt and the tool paths are set: a plain
// interactive bash reads ~/.bashrc, which belongs to the account and may not
// exist. Falls back to /bin/sh, where ENV does the same job.
func ShellArgv(root string) []string {
	if fi, err := os.Stat("/bin/bash"); err == nil && !fi.IsDir() {
		return []string{"/bin/bash", "--rcfile", RCPath(root), "-i"}
	}
	return []string{"/bin/sh", "-i"}
}

// LoginShellEnv adds what /bin/sh needs to read the startup file, for
// machines without bash.
func LoginShellEnv(root string) map[string]string {
	return map[string]string{"ENV": RCPath(root)}
}

// RCDir is exposed for tests.
func RCDir(root string) string { return filepath.Dir(RCPath(root)) }
