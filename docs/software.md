# The software a job gets

Every job and login session starts from the same small base image, and you add
what you need on top with `uv`, into your own home directory.

## The base set

About seventeen packages, the same on every machine:

```console
$ shome admin env packages
software in every session (17 packages)

PACKAGE          SOURCE
ca-certificates  shome
curl             shome
file             shome
gh               shome
git              shome
git-lfs          shome
...
```

In full: `ca-certificates`, `curl`, `wget`, `git`, `git-lfs`, `gh`, `jq`,
`less`, `nano`, `vim`, `openssh-client`, `rsync`, `zip`, `unzip`, `procps`,
`file`, `tree`. Plus the shome commands themselves, and `uv`.

Each earns its place: `ca-certificates` because every HTTPS fetch fails without
it, `less` because `git log` is unusable without a pager, `gh` for private
repos, an editor because a shell without one is a strange thing.

An admin adds what this cluster happens to need, and additions layer on top of
the base set rather than replacing it, so a script that works on one machine
works on the next:

```bash
shome admin env packages add build-essential
shome admin env install          # rebuild the image
shome admin env status           # what a session offers here
```

## Anything you install yourself goes through uv

`uv` needs no privileges, installs its own Python interpreters, and can be
pointed entirely inside one directory. Every
path it writes to is redirected under your home, so an environment you build is
your files: it counts against your disk limit and moves with `shome fs`.

```bash
uv venv                    # activated automatically next time
uv pip install numpy pandas
uv run script.py
uv tool install ruff       # onto your PATH
uv python install 3.13
```

## One home directory, one environment per kind of machine

You have a single home directory, and everything in it is shared by every job you run. Compiled software cannot be: a
Linux interpreter will not run on macOS, and a GPU job on a Mac runs on macOS.

So each *kind* of machine gets its own environment inside that one home
directory, named after what it is:

```console
$ echo $SHOME_SLOT $SHOME_VENV
linux-arm64 /home/alice/.shome/linux-arm64/venv
```

## On Linux there is no image

A Linux machine's jobs run in a namespace over that machine's own `/usr`, so
the base set is whatever it has installed. A freshly added Debian box has
almost none of it, so a node installs the base packages with its own package
manager when it joins, and only when that needs no password. A machine that
would need one is left alone and told what to run, and `shome admin env status`
reports what is missing.
