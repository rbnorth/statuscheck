# statuscheck

A small Go command-line tool that checks the public status pages for **GitHub**, **Bitbucket** and **Azure** and reports whether everything is working, what's degraded, and any open incidents or scheduled maintenance.

| Service   | Status feed | Format |
|-----------|-------------|--------|
| GitHub    | https://www.githubstatus.com/api/v2/summary.json | Atlassian Statuspage JSON |
| Bitbucket | https://bitbucket.status.atlassian.com/api/v2/summary.json | Atlassian Statuspage JSON |
| Azure     | https://azure.status.microsoft/en-us/status/feed/ | RSS |

### About Azure

Azure's public feed works differently from the other two. It lists **only active incidents** and is empty when nothing is wrong. That means:

- Azure shows either "No active incidents" or the incidents with their links. It has no per-component list, so `-v` adds nothing for Azure.
- Any active incident counts as a problem (exit code `1`), even if it's in a region or service you don't use.
- The public feed covers broad, widespread outages. Problems limited to your own subscription appear only in **Azure Service Health** in the Azure portal, which needs a sign-in and isn't checked here.

No API keys or sign-in are needed, and the tool uses only Go's standard library.

## Requirements

- Go 1.18 or newer

## Setup

```bash
go mod init statuscheck   # only needed once, if there's no go.mod yet
go build -o statuscheck .
```

Or run it without building:

```bash
go run .
```

## Usage

```bash
./statuscheck [flags]
```

| Flag       | Default | Description |
|------------|---------|-------------|
| `-s`       | `all`   | Services to check, comma-separated: `github`, `bitbucket`, `azure`, or `all`. For example `-s github,azure`. Names aren't case-sensitive. |
| `-v`       | off     | List every component, not just the ones that aren't operational. |
| `-json`    | off     | Print the raw results as JSON instead of the readable summary. |
| `-timeout` | `10s`   | How long to wait for the status pages before giving up (e.g. `5s`, `30s`). |

### Examples

```bash
./statuscheck                      # check every service
./statuscheck -s github            # only GitHub
./statuscheck -s azure             # only Azure
./statuscheck -s github,azure      # GitHub and Azure
./statuscheck -s github -v         # GitHub, listing every component
./statuscheck -json | jq .         # JSON output for scripts or monitoring
./statuscheck -timeout 5s          # give up after 5 seconds
```

### Example output

```
GitHub
  ✓ All Systems Operational
  updated Mon, 23 Nov 2026 08:40:12 MST · https://www.githubstatus.com

Bitbucket
  ! Minor Service Outage
    - Pipelines                                partial outage
  Incident: Delays in Pipelines builds [investigating, impact: minor]
      https://stspg.io/xxxxxxx
  updated Mon, 23 Nov 2026 08:38:55 MST · https://bitbucket.status.atlassian.com

Azure
  ✓ No active incidents
  updated Mon, 23 Nov 2026 08:30:00 MST · https://azure.status.microsoft/en-us/status/
```

Colors are shown when writing to a terminal. They're turned off automatically when the output is piped or redirected, or when the `NO_COLOR` environment variable is set.

## Exit codes

Useful for cron jobs, CI pipelines, and monitoring checks:

| Code | Meaning |
|------|---------|
| `0`  | All checked services are fully operational. |
| `1`  | At least one service is degraded, has an open incident, or is under maintenance. |
| `2`  | A status page couldn't be reached or read, or an unknown service name was given to `-s`. |

Example in a shell script:

```bash
if ! ./statuscheck -s github > /dev/null; then
  echo "GitHub is having problems — check https://www.githubstatus.com"
fi
```

## Moving off Bitbucket

When Bitbucket no longer matters, change the default in `main.go` so only GitHub is checked:

```go
const defaultServices = "github"
```

To check several services by default, list them: `const defaultServices = "github,azure"`.

`-s bitbucket` and `-s all` will still work if you need them. To remove Bitbucket entirely, delete its line from the `services` list.

## Adding another service

Any status page hosted on Atlassian Statuspage works. Add a line to the `services` list in `main.go`:

```go
{Key: "jira", Name: "Jira", Kind: kindStatuspage,
	URL: "https://jira-software.status.atlassian.com/api/v2/summary.json"},
```

Then check it with `-s jira`.
