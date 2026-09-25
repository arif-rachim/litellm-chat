# lchat

A mini coding agent for the terminal, a small version of Claude Code. A single static Go binary (~7 MB, no dependencies). Models are called through LiteLLM or any other OpenAI-compatible endpoint. Designed for small models such as Qwen3.5-35B-A3B.

The harness is what keeps small models on track:

- **Knows the environment.** The OS, installed tools, project type, `scripts` in `package.json`, and the file tree are sent up front. The model doesn't need to waste steps exploring.
- **Reminds the model when it makes mistakes.**
  - Broken JSON arguments are repaired automatically.
  - Tool calls written as text are converted into real tool calls.
  - Wrong tool names or arguments are answered with a correction formatted as *Problem / Why / Next step / Example*.
  - If `edit_file` doesn't match, the harness shows the most similar snippet of the file.
  - Exactly repeated actions are detected as a loop.
  - The same formatting mistake 3x in a row ends the turn.
- **Searches for a solution instead of going in circles.** What separates a small model that "experiments with direction" from one that "keeps repeating itself":
  - **Attempt log.** Every step and its result is stored concisely and sent with every request, inside a system message that is always rebuilt — so it can't be lost when the context is trimmed. The model always sees what has already failed.
  - **Failure signatures.** Failed output is summarized into a signature that is robust to line numbers and file names, so five different commands hitting the same wall still count as one wall.
  - **Escalation ladder.** Each repetition changes the *shape* of the task rather than adding the same scolding: (2×) write down which guess was proven wrong and one new hypothesis → (3×) must switch the kind of step, e.g. read the file before editing again → (4×) must `ask_user` with concrete options → after that the turn is stopped with a summary of what was tried.
  - A passing verification clears the streak: the search is considered to start over.
  - **Oscillation is recognized.** A file returning to content it already had in that turn (A→B→A) means the model is undoing its own work — it is immediately asked to name which assumption was wrong, rather than trying again.
  - **No-progress budget.** Six steps without new evidence — no new file read, no change, no new kind of failure, no passing verification — and the harness asks what is missing, then tells it to ask the user, then stops. Running `ls` and `echo` is not progress.
  - **Read before the second edit.** After one successful `edit_file`, the next edit to the same file is rejected until that file is re-read or verified — the copy in the model's context is already stale.
- **`/task`: decomposition with a done-condition.** For truly large tasks, `/task <request>` (or `lchat --task -p`) asks a planner to break it into 2-8 subtasks, each with **one `verify` command** — and a subtask can only be closed after the harness itself sees that command exit 0. Each subtask runs in a clean context with a small step budget; only facts recorded by the harness carry over. A subtask whose verifier is already green before it starts is skipped ("red first"); a subtask that doesn't fit is split by the harness via the planner; two consecutive failures discard the plan and fall back to a single regular turn. The planner may answer "this is one step", so a false trigger only costs one call. `LCHAT_PLANNER_MODEL` uses a different model for planning; empty = the main model.
- **A context that never spans more than one milestone.** As soon as a `todo` step is closed with a change that passes verification, the harness compacts that turn's context back to the original request. Only the *handoff* recorded by the harness carries over — completed steps, changed files, passing verification commands, paths that were read — not the transcript. The small model always works in a window that fits in its head, without a separate planner.
- **Reasoning.**
  - Adaptive thinking: on while planning and after errors, off during routine steps.
  - The work plan (`todo`) is recalled at every step. The list belongs to the model, **the checkmarks belong to the harness**: a step can only be marked done if its change has been verified, and a shrinking list is announced.
  - After a failure, the model is asked to reflect on the cause first.
  - **Done is decided by exit code, not by claims.** If the model says it's done without verifying, the harness itself runs the project's verification command (`go test`, `npm test`, …) through the same permission gate. Pass → the answer is accepted; fail → the output goes back to the model as evidence. What counts as "verification" is strict too: running `node x.js` is not verification, `npm test` is.
- **Asks back.** Through the `ask_user` tool, the model can ask multiple-choice or free-form questions when the decision is in your hands, instead of guessing.
- **Three work modes** that can be switched with Tab: `plan`, `ask`, and `auto`.
- **Images.** Screenshots can be sent to the model via Ctrl+V, `/img`, pasting a file path, or typing its path in the message. Attachments are always named in the text so that small models are aware there is an image, and if the model calls `read_file` on an image file, the harness attaches the image instead of answering "binary file".
- **Auto-check** after writing or editing a file: `node --check`, JSON validation, and `py_compile`. `tsc` only runs with `--check-ts`.
- **Session log.** Every session is written to `~/.local/state/lchat/sessions/<time>.jsonl`: every request along with what the harness added, every reply and tool call, every tool result, every harness correction, every screen line. It has one purpose: to be taken into analysis to see where the model (or its harness) wastes steps. `/log` shows its path; `--no-log` or `LCHAT_LOG=off` turns it off. It contains tool output, so don't share it raw.
- **Model profiles.** Everything model-specific (how to switch thinking, sampling, tool call format, context size) lives in a profile. If you switch models, run `lchat probe`.

## Distribution

The build output is **a single self-contained file**: statically linked, with no runtime, libraries, or companion files. Just copy the file to the target machine.

```bash
./release.sh v0.1.0      # or: make release
```

The output is in `dist/`:

| File | Size | For |
|---|---|---|
| `lchat-linux-amd64` | 7.2 MB (3.1 MB gzipped) | Intel or AMD PCs/servers |
| `lchat-linux-arm64` | 6.7 MB (2.8 MB gzipped) | Raspberry Pi, ARM servers |
| `SHA256SUMS` | | for verifying the download |

On the target machine:

```bash
install -Dm755 lchat-linux-amd64 ~/.local/bin/lchat
lchat -version
lchat config        # set up endpoint, key, and model
```

Something to keep in mind when sharing: don't copy `.env` or `~/.config/lchat/config.json` along with it, because both contain API keys. The binary itself doesn't contain any key.

## Install

```bash
sudo apt install golang-go      # Go 1.22+ (or use an existing Go)
make install                    # build then copy to ~/.local/bin/lchat
```

Without `make`:

```bash
CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o lchat . && install -Dm755 lchat ~/.local/bin/lchat
```

## Configuration

The easiest way: run the wizard.

```bash
lchat config        # or type /config inside a session
```

The wizard asks four things, in order:

1. **Endpoint**: OpenRouter directly, or LiteLLM / another OpenAI-compatible endpoint (you fill in the address yourself).
2. **API key**, typed masked. Enter means use the saved one.
3. **Model**: the list is fetched automatically from the endpoint using that key, complete with context size, tool and image support, and price per 1 million tokens. In this picker **numbers select, text filters**: typing `qwen` only narrows the list; what actually selects is the number typed afterwards. `n`/`p` change pages, `=id` forces an id that isn't in the list, an empty Enter cancels.
4. **Probe**: optional, to detect how that model turns on thinking, then save it as a profile.

The result is saved in `~/.config/lchat/config.json` with `600` permissions, and is used immediately by the running session.

### Configuration sources and their order

From highest priority: command-line flags, environment variables, the `.env` file, then `config.json` from the wizard. If something overrides the wizard's result, lchat tells you when saving.

| Env | Default | Description |
|---|---|---|
| `LCHAT_BASE_URL` | `http://localhost:4000` | LiteLLM proxy (or another OpenAI-compatible endpoint) |
| `LCHAT_API_KEY` | | LiteLLM master key / API key |
| `LCHAT_MODEL` | `qwen3.5-35b-a3b` | model name/alias |
| `LCHAT_CTX` | from profile | context limit (tokens) |
| `LCHAT_TOOL_MAX` | `8000` | byte limit of tool output sent to the model |
| `LCHAT_TEMPERATURE` | from profile | temperature override |
| `LCHAT_MODELS` | `~/.config/lchat/models.json` | model profile file |
| `LCHAT_LOG` | (on) | `off` disables the session log |
| `LCHAT_LOG_DIR` | `~/.local/state/lchat/sessions` | session log folder (JSONL, `0600`) |
| `LCHAT_CONFIG` | `~/.config/lchat/config.json` | file produced by `/config` |

**The `.env` file.** All variables in this table, plus `OPENROUTER_API_KEY`, can be put in `.env`. lchat looks for it in order in the working folder, next to the `lchat` binary, then in `~/.config/lchat/.env`. Environment variables that are already set still win. Its contents are not passed on to commands run by the agent. Copy `.env.example` as an example. The `.env` file is already in `.gitignore`.

**Directly to OpenRouter without LiteLLM.** If `LCHAT_BASE_URL` and `LCHAT_API_KEY` are not set but `OPENROUTER_API_KEY` is, lchat automatically uses `https://openrouter.ai/api/v1` with the default model `qwen/qwen3.5-35b-a3b`.

Example LiteLLM configs (vLLM, Ollama, OpenRouter) are in `litellm.config.example.yaml`.

## Usage

```bash
lchat                                   # interactive mode
lchat -p "test-nya gagal, perbaiki"     # one-shot: answer to stdout, activity to stderr
lchat --yolo -p "..."                   # no permission prompts (be careful)
lchat config                            # set up endpoint, key, and model (guided)
lchat probe -m qwen3.8-27b --save       # detect a new model's behavior, save its profile
```

Flags: `-m`, `-p`, `-i <image>`, `--task`, `--planner-model`, `--subtask-steps 8`, `--yolo`, `--mode plan|ask|auto`, `--max-steps 30`, `--think auto|on|off`, `--quiet-think`, `--check-ts`, `--raw`, `-v`.

REPL commands: `/config`, `/clear`, `/task <request>`, `/mode [name]`, `/img <path>`, `/paste`, `/model [filter]`, `/profile`, `/think [auto|on|off]`, `/env`, `/help`, `/exit`. Type `/` then Tab to complete command names; the list of candidates appears on its own as you type. `/model` opens the same picker as the wizard, and `/model qwen` only filters it. End a line with `\` for multi-line input.

### Work modes

Press **Tab** while typing to switch `plan → ask → auto`; the prompt changes color and label accordingly.

| Mode | write_file & edit_file | bash | What it's for |
|---|---|---|---|
| `plan` | blocked | asks permission | the model investigates first, then presents a plan. Approve it with a single `y` key, and the mode automatically switches to `auto` |
| `ask` (default) | asks permission | asks permission | everyday work |
| `auto` | runs directly | asks permission | when you already trust the direction of the work |

Risky actions (see the Security section) are still asked about in every mode.

### Images

Four ways to attach an image to the next message:

```bash
lchat -i screenshot.png -p "kenapa layoutnya rusak?"   # one-shot
```

- **Ctrl+V** inside the REPL: lchat reads the clipboard itself. Requires `wl-clipboard` (Wayland) or `xclip` (X11) to be installed: `sudo apt install wl-clipboard`. If the clipboard contains text, the text is typed into the line; if it contains an image, the image is attached.
- **`/img <path>`** — can take several paths at once.
- **Paste or drag a file** into the terminal: image paths on the line are automatically attached and removed from the message text.

Things to keep in mind: the model must support images (all Qwen3.5 and Qwen3-VL do; if not, lchat tells you when the server rejects it). The limit per image is 5 MB. Images are the heaviest load on the context, so when the context starts filling up, old images are dropped first and replaced with a note.

### Keys

In a real terminal, input uses its own raw mode: left/right arrows, up/down arrows for history, Ctrl+A/E, Ctrl+U, Ctrl+W, Ctrl+V for images, and Tab to change mode. Permission prompts need just a single `y`, `n`, or `a` key without Enter. Ctrl+C cancels the running turn; twice in a row exits; Ctrl+D also exits. If input is not a terminal, everything automatically falls back to plain line mode.

## Security

lchat does not use a sandbox; commands run with your account's privileges. There are three layers of safeguards:

1. **Work mode.** In `plan`, tools that modify files are blocked until you approve the plan.
2. **Per-tool permission.** `bash`, `write_file`, and `edit_file` always ask for permission. Option `a` means always allow that tool for the session. With `--yolo`, these permissions are skipped.
3. **Mandatory confirmation for risky actions.** The actions below are asked about every time, even when using `--yolo` or after choosing `a`:
   - reading, writing, or listing outside the project folder, including via symlinks;
   - touching secret files: `.env`, `*.pem`, `*.key`, `id_rsa`, `~/.ssh`, `~/.aws`, `.npmrc`, and the like;
   - commands such as `rm -r`/`rm -f`, `sudo`, `git push`, `git reset --hard`, `curl … | sh`, `chmod -R`, and `npm publish`.

   If there is no terminal to ask, for example in piped `-p` mode, the action is blocked outright.
4. **If permission is denied, the turn stops.** The model doesn't try another way until you give a new instruction.

Keys from `.env` are not passed on to commands run by the agent. The list above is a safety net, not a guarantee, because commands can be disguised. So read what you approve first, and don't use `--yolo` in important folders.

## Switching models

Available built-in profiles:

| Profile | Suitable for | How thinking is switched |
|---|---|---|
| `qwen3*` | Qwen3, 3.5, 3.8, … | `chat_template_kwargs.enable_thinking` (vLLM/SGLang) |
| `qwen3*` + OpenRouter URL | Qwen via OpenRouter | `reasoning.enabled` |
| `*qwen3*instruct*` | instruct variants | no thinking |
| `*thinking*` | thinking variants | always thinking |
| `*` | other models | no switch |

For other models or backends, run:

```bash
lchat probe -m <model> --save
```

The probe tries every way of switching thinking, checks whether reasoning comes out as a `reasoning_content` field or a `<think>` tag, then tests native tool calls. The result is saved as a profile in `~/.config/lchat/models.json`. That file can be edited manually (`//` comments are allowed):

```jsonc
{"profiles": [{
  "match": "qwen3.8*",                 // model name glob
  "match_url": "*localhost:4000*",     // optional: only for this endpoint
  "thinking": {"control": "chat_template_kwargs", "kwarg": "enable_thinking", "history": "drop"},
  "sampling": {"think": {"temperature": 0.6, "top_p": 0.95}, "no_think": {"temperature": 0.7, "top_p": 0.8}},
  "tool_format": ["native", "hermes", "qwen_xml", "json_block"],
  "ctx": 65536
}]}
```

Available `control` values: `body` (`on_body`/`off_body` merged into the request), `chat_template_kwargs`, `reasoning_effort`, `prompt_switch` (`/think`, `/no_think`), `always`, and `none`.

## Development

```bash
make test     # go vet + unit tests (including fake LLM server & probe)
make build
```

### Design documentation

If you (or the next agent) are going to change the code, start from
[`docs/harness.md`](docs/harness.md) — the reasoning behind the shape of the agent loop,
the invariants that must not be broken, and what has deliberately not been done yet.
The index is in [`docs/`](docs/README.md).
