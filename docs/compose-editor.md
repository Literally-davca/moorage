# Compose Editor, Linter & Experience Modes

Moorage adapts to how you prefer to author stacks without locking you in.

## User Experience Modes (`Beginner | Advanced | Custom`)

On first sign-in, Moorage asks whether you want **Beginner (Guided)** or **Advanced (YAML-first)**. You can change modes or toggle any of the 6 switches anytime under **Account → Experience & editor preferences**:

1. **Default Compose view**: `Form (lossless)` | `YAML (with comments)` | `YAML (clean)`
2. **Default `.env` view**: `Variables table` | `Raw .env text`
3. **New stack starting point**: `Templates catalog first` | `Blank Compose editor`
4. **Linter tips expanded by default**: on / off
5. **In-app guides & setup hints**: `Extended` | `Compact`
6. **Action confirmations**: `All actions (stop, restart, pull, delete)` | `Destructive actions only`

Changing any individual switch automatically transitions your profile to **Custom**.

## Lossless Compose Views (`Form | Comments | Clean`)

- **Form**: structured cards per service (`image`, `restart`, `ports`, `volumes`, `environment`, `networks`, `depends_on`, `mem_limit`, `healthcheck`, `moorage.url`) backed by a YAML AST parser, so comments and advanced keys (`deploy`, `logging`, `cap_add`, etc.) remain untouched.
- **Comments**: full `compose.yaml` with inline and block `#` comments.
- **Clean**: hides all comments and strips original blank lines (outside `|`/`>` block scalars) for compact vertical reading. Untouched lines keep their comments when saved; any new blank lines you type while editing in Clean view are preserved.

## Auto-Indent on Paste & Fix Indent

When pasting unindented Compose snippets (stripped of leading spaces or using tabs) at root column 0 in YAML mode, Moorage reconstructs standard 2-space Compose indentation automatically and offers an inline **Undo** bar (as well as standard `Ctrl+Z`). You can also trigger reconstruction explicitly via the **Fix indent** toolbar button.

## Linter 1-Click Quick Fixes

Alongside errors and warnings, informational linter **Tips** offer 1-click fixes directly in `compose.yaml` / `.env`:

- **Pin tag**: looks up the latest stable SemVer tag in the container registry (`:latest` -> e.g. `:1.27.4-alpine`).
- **Add memory limit**: adds `mem_limit: 512m` safely.
- **Add healthcheck**: inserts a sensible `healthcheck` block tailored to the service image/ports.
- **Add to `.env`**: appends missing `${VAR}` keys discovered in Compose.

## Generated values

Write `@generate(password, 32)`, `@generate(port, 8080)` or a Coolify name such as `${SERVICE_PASSWORD_DB}` and Moorage fills `.env` when you save. See [generated-values.md](./generated-values.md).
