# Setup

Target: an Ubuntu/Debian host with systemd (e.g. `shb`). All commands are one-liners.

## 1. Credentials

| Secret | Where to get it |
|---|---|
| `CLAUDE_CODE_OAUTH_TOKEN` | run `claude setup-token` on a machine with a browser (needs a Pro/Max plan). The token is valid for one year. |
| `TELEGRAM_BOT_TOKEN` | Telegram: talk to @BotFather → `/newbot` |
| `X_CONSUMER_KEY`, `X_CONSUMER_SECRET`, `X_ACCESS_TOKEN`, `X_ACCESS_TOKEN_SECRET` | developer.x.com → your app → "Keys and tokens". The app needs Read permission. Generate the access token for your own account. Buy API credits (pay-per-use). |

Telegram chat id: send any message to your new bot, then run:

```sh
curl -s "https://api.telegram.org/bot<TOKEN>/getUpdates" | grep -o '"chat":{"id":[0-9-]*'
```

## 2. Install the Claude Code CLI (on the host)

Needs Node.js 22 or newer:

```sh
sudo npm install -g @anthropic-ai/claude-code && claude --version
```

Auto-update is disabled for newsagent's runs. To update by hand:

```sh
sudo npm update -g @anthropic-ai/claude-code
```

## 3. Build and copy

```sh
make build && scp newsagent deploy/newsagent-* shb:/tmp/
```

## 4. Install (on the host)

```sh
sudo install -m 0755 /tmp/newsagent /usr/local/bin/ && sudo install -m 0644 /tmp/newsagent-*.service /tmp/newsagent-*.timer /etc/systemd/system/ && sudo systemctl daemon-reload
```

```sh
sudo install -d -m 0755 /etc/newsagent && sudo install -m 0600 /dev/null /etc/newsagent/secrets.env && sudoedit /etc/newsagent/secrets.env
```

Fill in `secrets.env` using `deploy/secrets.env.example` as a template. The file stays root-only because systemd reads it before dropping privileges.

Copy `deploy/config.example.json` to `/etc/newsagent/config.json` (mode 0644). Then set `telegram.chat_id`, replace or remove the `REPLACE_OR_REMOVE` entry in `x.accounts`, and adjust `llm.interests`. newsagent refuses to start while either placeholder is left.

## 5. First run and enable

```sh
sudo systemctl start newsagent-poll.service; journalctl -u newsagent-poll -n 50 --no-pager
```

```sh
sudo systemctl enable --now newsagent-poll.timer newsagent-digest.timer
```

Optional hourly mode (short updates 09:00–23:00, on top of the daily digests):

```sh
sudo systemctl enable --now newsagent-hourly.timer
```

Turn it off again with `sudo systemctl disable --now newsagent-hourly.timer`.

## Operations

- Logs: `journalctl -u newsagent-poll -u newsagent-digest -u newsagent-hourly -f`
- Send a digest now: `sudo systemctl start newsagent-digest.service`
- Data: `/var/lib/newsagent/newsagent.db` (SQLite; items are pruned 30 days after they were fetched; items without a publish date after a year, so they are not seen as new again)
- Renew `CLAUDE_CODE_OAUTH_TOKEN` yearly (`claude setup-token`). Until then, polls log `claude cli` errors.

## Config reference

| Key | Default | Meaning |
|---|---|---|
| `db_path` | `/var/lib/newsagent/newsagent.db` | SQLite database file |
| `timezone` | `UTC` | zone for times shown in digests |
| `max_age` | `48h` | ignore items published earlier than this; must be under 30 days (the retention) |
| `feeds` | `[]` | RSS/Atom feeds: `{"name": …, "url": …}`; names unique and not `hn` or `x`, URLs `http(s)://` |
| `hn.enabled` | `false` | read Hacker News top stories |
| `hn.top_n` | `30` | number of HN top stories to consider (1..500) |
| `x.enabled` | `false` | read X (needs the four `X_*` secrets) |
| `x.max_posts_per_poll` | `25` | 5..100, per request |
| `x.max_posts_per_day` | `200` | hard daily cap (UTC day) on billed reads |
| `x.min_interval` | `1h` | minimum time between X reads |
| `x.mentions` | `false` | ping every mention of you |
| `x.accounts` | `[]` | usernames (without `@`) whose posts always go into the digest |
| `x.home_timeline` | `true` | read the home timeline |
| `x.exclude_replies` | `false` | skip replies in the home timeline and the `x.accounts` feeds |
| `llm.claude_path` | `claude` | Claude Code CLI binary |
| `llm.home_dir` | `/var/lib/newsagent` | writable HOME for the CLI |
| `llm.model` | `claude-opus-5-5` | Claude model for scoring and digests |
| `llm.score_effort` / `llm.digest_effort` | `low` / `medium` | Claude effort levels |
| `llm.interests` | — (required) | what you care about; Claude scores every item against it |
| `thresholds.urgent` / `thresholds.digest` | `9` / `6` | minimum score for a ping / a digest line |
| `thresholds.max_urgent_per_poll` | `3` | cap on pings per poll |
| `telegram.chat_id` | — (required) | chat to send to: a number (negative for groups) or `@channelname` |

The digest schedules live in `newsagent-digest.timer` and `newsagent-hourly.timer` (`OnCalendar=`).
