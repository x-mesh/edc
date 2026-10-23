**English** | [한국어](remote.ko.md) · [Back to README](../README.md)

# Remote tasks and SSH tunnels

[Remote recipes](#remote-recipes) · [SSH tunnels](#ssh-tunnels) · [Connection test](#connection-test)

## Remote recipes

`edc remote <group>` executes a YAML recipe on an inventory group. It uses local OpenSSH configuration, agents, and known host checks.

Each command loads the remote account default shell in interactive mode. Shell startup output and prompt hooks stay hidden.

Store no passwords or private keys in inventory and recipe files. Configure SSH aliases in `~/.ssh/config`.

Each step needs `name` and either `command` or `upload`. The `verify` field is optional. If `verify` is absent, the action result decides the step result.

Use `upload` instead of `command` to send a local file. Use `source` for the local file and `destination` for the remote path. The `mode` field is an optional octal permission.

`upload` runs OpenSSH `scp`. If you set `mode`, `edc` runs remote `chmod` after the transfer.

Keep `name` for group references. If `target` is absent, `edc` uses `name` as the SSH target.

![edc remote daily --dry-run prints the host by step plan table without an SSH connection and marks an unmatched step with –](media/remote.gif)

The demo shows the tags of a recipe and the plan table that `--dry-run` prints.

### Command shape

The group is a positional argument. The `--group` flag is an alias for the same value. Use only one of the two.

```bash
cp examples/remote/inventory.yaml ./inventory.yaml
edc remote              # select the group, then confirm
edc remote daily        # name the group, then confirm
edc remote daily -f     # name the group and skip the confirmation
```

`edc` finds `inventory.yaml` in these directories, in this order:

1. `./.edc/`
2. `./`
3. `os.UserConfigDir()/edc/`. This path follows the operating system.

The `recipe.yaml` file uses the same order. The file selectors list the YAML files of the same directories, in the same order.

Keep the remote files of a project in `./.edc/`. Then the project root stays clean. To keep the directory out of git, put a `.gitignore` file in it. `edc` does not make this file.

```bash
mkdir -p .edc
printf '*\n' > .edc/.gitignore
```

If you want to commit the files, remove `.edc/.gitignore`.

A relative `upload.source` path starts from the directory where you run `edc`. It does not start from the directory of the recipe file.

Add `-v` to show the search order. The line marks a directory that does not exist. It also marks a `./.edc/` directory that has no `.gitignore` file. `--list -v` shows the same line.

```
inventory  ./.edc/inventory.yaml      recipe  ./.edc/recipe.yaml
search     ./.edc/  →  ./  →  /home/me/.config/edc/ (missing)
```

The `--inventory` and `--recipe` flags win over the found files.

If you name a group, `edc` asks no path questions. It shows the plan and requests the confirmation only.

If you name no group, `edc` selects the group first. It then shows the inventory path, selects the recipe, and requests confirmation.

After the selection, the plan starts with an `edc remote` command. The command names the group, the inventory, and the recipe that you selected. It also keeps the flags that you gave. Run this command to use the same selection again. `edc` then requests the confirmation only.

The interactive selectors draw the list in place. Use the up and down arrow keys, or `j` and `k`. Press Enter to select. Press `q` or Esc to cancel. A cancelled selection returns exit code `4`.

While you choose, `edc` reverses the colors of the row under the cursor and puts a `▌` bar on its left. The reverse makes the row clear in a black and white terminal too.

The list stays on the screen after you select. The bar stays on the item you chose, without the reverse, and the first line keeps the name of the question.

```
inventory 파일
  lab-hosts.yaml  ·  group 1개, host 1개
▌ prod-hosts.yaml  ·  group 2개, host 2개
```

If `edc` finds no `inventory.yaml`, it lists the YAML files of the search directories that read as an inventory. The list shows the group and host counts. The recipe list shows the recipe name and the step count. `edc` hides a YAML file that does not read as an inventory or a recipe. If the list is empty, `edc` asks for a path.

The confirmation questions put the question, the two answers, and the key help on one line. Move with the left and right arrow keys and press Enter, or press `y` or `n` to answer at once. The answer you point at gets a `▌` bar and reversed colors. The default answer is no.

Use `-f` or `--force` to skip the confirmation. Combine it with `-v` for streaming output. If you name no group, `-f` needs an inventory with exactly one group.

A recipe command runs without a terminal, and its input is empty. If a step runs a `host changes` command without `--yes`, the command cannot get a confirmation. `edc disk grow` stops with exit code `4`. `edc route switch` restores the route. `edc change apply` rolls back when its timer fires.

If a step adds `--yes`, `edc disk grow` and `edc change apply` run on every host of the group without a question. `edc route switch` does the same when the exit identity matches. Read the plan before you confirm. Do not run such a recipe with `-f`.

These group names are reserved for future subcommands: `run`, `list`, `plan`, `hosts`, `groups`. An inventory that uses one of them fails to load.

The earlier `edc remote run` form is gone. Use `edc remote <group>`.

### Dry run and inventory listing

Use `-n` or `--dry-run` to print the plan and exit. `edc` opens no SSH connection. Add `--json` to get the plan as JSON.

```bash
edc remote daily --dry-run
edc remote daily --dry-run --json -
```

Use `-l` or `--list` to print the groups and hosts of the inventory. Name a group to limit the output to that group.

```bash
edc remote --list
edc remote daily --list --json -
```

The `--dry-run` and `--list` options do not combine with `-f`. Add `--redact` to hide IP addresses in the output.

### Edit the inventory and the recipe

Run `edc setup remote` in a terminal to edit the inventory and the recipe on one screen.

```bash
edc setup remote
edc setup remote --inventory ./.edc/inventory.yaml --recipe ./.edc/recipe.yaml
```

The command opens the files that `edc remote` finds. If a file does not exist, the command creates `./.edc/inventory.yaml` or `./.edc/recipe.yaml` on the first save. Use `--inventory` and `--recipe` to name other files. The `defaults.remote.inventory` and `defaults.remote.recipe` settings also name files, and the flags win.

The screen has a list of hosts and a list of groups. Press `a` to add, `e` to edit, `d` to delete, `g` or `h` to change the list, `s` to save, and `q` to quit. In the group list, press `e` and use the space bar to pick the hosts of a group. Press `p` to set `parallel`, and `o` to set `parallel` for the selected group.

Press `r` to open the list of steps. Press `a` to add a step, `e` to edit it, `d` to delete it, and `K` or `J` to move it up or down. Press `n` to name the recipe. A step has a command or an upload. Give one of them, not both.

Before a save, `edc` checks the result with the rules of `edc remote`. A save writes only the files that you changed. The save keeps the comments of the file, but a renamed host or step loses its comments. A new file has mode `0600`. If the file changed after `edc` read it, the save stops and `edc` writes nothing. If you discard unsaved changes, the command returns exit code `4`.

The screen stores no passwords or keys. Use an SSH alias as the `target`.

### Host tags

Add `tags` to a host and to a step. A step without `tags` runs on every host of the group. A step with `tags` runs only on the hosts that carry one of the same tags. Use this to send different work to macOS and Linux hosts in one recipe.

```yaml
# inventory.yaml
hosts:
  - name: build-server
    tags: [linux]
  - name: workstation
    tags: [mac]
```

```yaml
# recipe.yaml
steps:
  - name: git-kit          # no tags, so every host runs this step
    command: git-kit update
    verify: git-kit --version
  - name: brew
    tags: [mac]
    command: brew update && brew upgrade
    verify: brew --version
  - name: apt
    tags: [linux]
    command: apt-get update && apt-get -y upgrade
    verify: apt-get --version
```

A host that does not match a step gets no result for that step. The report keeps the skip count for failed hosts only.

If no host in the group matches the tags of a step, `edc` prints a warning to stderr and continues. Check the tag spelling.

### One table

`edc remote` uses one table for the plan and the results. The rows are the hosts and the columns are the steps. The table starts with `·` in every cell, and each cell changes when the step of that host ends. A cell shows `–` if the tags of the step do not match the host.

```
edc remote  daily  ·  host 3  ·  step 3  ·  실행 8
inventory  ./inventory.yaml      recipe  ./recipe.yaml

host          git-kit  x-mesh  brew
build-server  PASS     PASS       –
ci-runner     PASS     ⠋          –
workstation   PASS     ·          ·

⠋  ci-runner / x-mesh / command  ·  4/8 완료  6.6s

git-kit  git-kit update  →  git-kit --version
x-mesh   xm update  →  xm version
brew     brew update && brew upgrade -f   tags mac
```

The line under the table names the host, the step, and the phase that runs now. The lines below it give the command of each step.

If stdin and stdout are terminals, the confirmation question appears right under this table, above the command list. The answer you point at gets a `▌` bar and reversed colors. Answer with the left and right arrow keys and Enter, or with `y` or `n`. The same line then becomes the progress line and the table fills in. Use `-f` to skip the question.

```
host   uname  uptime
alpha  ·      ·

실행할까요?     예   ▌ 아니오       ←/→ 이동   Enter 선택   y/n 바로 답하기
```

The table fits the terminal width. `edc` first shortens the column names, then changes `PASS` and `FAIL` to `✓` and `✗` with a legend, and last shortens the host names.

Add `-v` to show the last output lines below the table.

Press Ctrl-C to cancel. `edc` stops the running commands, marks the remaining steps as `SKIP`, prints the summary, and returns exit code `4`. Press Ctrl-C again to close the screen at once.

`edc` prints the earlier result lines instead of the table if stdin or stdout is not a terminal, if `--json` is set, or if `NO_COLOR` is set.

### Automation

Use all flags for cron or launchd. A non-terminal command never waits for input. It also skips the confirmation.

```bash
edc remote daily \
  --inventory ./inventory.yaml \
  --recipe ./examples/remote/daily-update.yaml \
  --parallel 2 \
  --json ./remote-report.json
```

A non-terminal command needs a group. Name it as the positional argument or with `--group`.

Use `-v` or `--verbose` to stream each remote command.

`edc` prints the PASS or FAIL line at the end of each step. The final output shows the failures and one summary line with the counts and the elapsed time.

Set `parallel` in the inventory to run hosts concurrently. Use `group_options.<group>.parallel` for one group.

The `--parallel` option overrides both inventory values. Each host still runs its steps in order.

Each host runs in inventory order. Each step runs its command and verify command in recipe order.

If a step fails, `edc` skips later steps on that host. The next host still runs. Any failure returns exit code `1`.

## SSH tunnels

`edc tunnel` starts, stops, and lists SSH tunnels. It runs the local OpenSSH `ssh` command in the background. It uses your OpenSSH configuration, agent, and known host checks.

Run `edc tunnel` in a terminal to open the tunnel screen. The screen lists each tunnel with its state. It refreshes every 2 seconds.

| Key | Action |
|---|---|
| `enter` | Start the selected tunnel. If it runs, stop it. |
| `n` | Open the form for a new tunnel. |
| `r` | Restart the selected tunnel. Only a saved tunnel has this key. |
| `x` | Remove the definition of the selected tunnel after a question. Only a saved tunnel has this key. |
| `q` | Quit. Running tunnels keep running. |

In the form, `tab` moves to the next field. `←` and `→` choose the host and the kind. `ctrl+t` tests the connection. `enter` starts the tunnel. The form shows the `ssh` command and a help text for the active field.

The `save` field decides if `edc` keeps the tunnel. `yes` is the default. `edc` writes the definition to `tunnels.yaml` and shows the path below the field. If no file exists, `edc` creates `./.edc/tunnels.yaml`. `no` starts the tunnel once. The tunnel leaves the list when it stops. A name that already exists in the file is not replaced.

Define the tunnels that you use often in `tunnels.yaml`. `edc` looks in `./.edc`, in the current directory, and in the config directory. `--file` sets the file.

```yaml
tunnels:
  db:
    host: jump1                  # an inventory host name or a ~/.ssh/config alias
    local: 5432                  # a port, or auto
    remote: db.internal:5432     # ssh -L
  api-dev:
    host: dev
    reverse: 8080:localhost:3000 # ssh -R, port:host:port
  proxy:
    host: jump1
    socks: 1080                  # ssh -D, a port, or auto
```

Each tunnel needs exactly one of `remote`, `reverse`, or `socks`. Local ports bind to `127.0.0.1`. `auto` picks a free port.

```bash
edc tunnel                                      # tunnel screen, or the list if no terminal
edc tunnel list --json -                        # the list as JSON
edc tunnel up db                                # start a defined tunnel
edc tunnel up -L auto:db.internal:5432 jump1    # start a tunnel that has no definition
edc tunnel up -L 5432:db.internal:5432 --name db jump1 --save   # start it and save the definition
edc tunnel test db                              # test the connection, start nothing
edc tunnel down db
edc tunnel down --all
```

## Connection test

`ctrl+t` in the form and `edc tunnel test <name>` run the same checks. They start no tunnel. The checks run in this order:

| Check | How |
|---|---|
| Local port | `edc` binds the port at `127.0.0.1` and looks for a running tunnel that uses it. |
| SSH login | `ssh host true` with `BatchMode`. |
| Destination (`remote`) | `ssh -W destination host`. The host connects to the destination, so the result shows what the host sees. |
| Remote port (`reverse`) | `ssh -R` with `ExitOnForwardFailure`. The host opens the port and closes it at once. |
| Local target (`reverse`) | `edc` opens a TCP connection to the target on this machine. A failure is a warning only. |

If the SSH login fails, `edc` skips the checks that need the host. `edc tunnel test` returns exit code `1` if a check fails. The form still lets you start a tunnel after a failed check, because a service can start later.

The list shows the state of each tunnel. The `saved` column shows `●` for a tunnel with a definition and `–` for a tunnel that runs once.

| State | Meaning |
|---|---|
| `up` | The SSH master process runs. |
| `down` | The tunnel has a definition and does not run. |
| `stale` | The state file exists, but the SSH process is gone. Press `enter` or run `edc tunnel down` to clear it. |

The `reach` column opens a TCP connection to the tunnel entrance. It shows `ok` if the destination keeps the connection open. It shows `refused` if the connection closes at once. Reverse tunnels show `-`.

`edc` keeps one state file for each tunnel in the `tunnels` directory next to the history database. `edc` does not store passwords or keys. The tunnel uses `BatchMode`, so use an SSH agent or a key without a passphrase.

