# Setup

Target: an Ubuntu/Debian host with systemd (e.g. `shb`). All commands are one-liners.

## 1. Credentials

| Secret | Where to get it |
|---|---|
| `ANTHROPIC_API_KEY` | console.anthropic.com → API keys |
| `TELEGRAM_BOT_TOKEN` | Telegram: talk to @BotFather → `/newbot` |
| `X_CONSUMER_KEY`, `X_CONSUMER_SECRET`, `X_ACCESS_TOKEN`, `X_ACCESS_TOKEN_SECRET` | developer.x.com → your app → "Keys and tokens". The app needs Read permission. Generate the access token for your own account. Buy API credits (pay-per-use). |

Telegram chat id: send any message to your new bot, then run:

```sh
curl -s "https://api.telegram.org/bot<TOKEN>/getUpdates" | grep -o '"chat":{"id":[0-9-]*'
```

## 2. Build and copy

```sh
make build && scp newsagent deploy/newsagent-* shb:/tmp/
```

## 3. Install (on the host)

```sh
sudo install -m 0755 /tmp/newsagent /usr/local/bin/ && sudo install -m 0644 /tmp/newsagent-*.service /tmp/newsagent-*.timer /etc/systemd/system/ && sudo systemctl daemon-reload
```

```sh
sudo install -d -m 0755 /etc/newsagent && sudo install -m 0600 /dev/null /etc/newsagent/secrets.env && sudoedit /etc/newsagent/secrets.env
```

Fill in `secrets.env` using `deploy/secrets.env.example` as a template. The file stays root-only because systemd reads it before dropping privileges.

Copy `deploy/config.example.json` to `/etc/newsagent/config.json` (mode 0644). Then set `telegram.chat_id` and adjust `llm.interests`.

## 4. First run and enable

```sh
sudo systemctl start newsagent-poll.service; journalctl -u newsagent-poll -n 50 --no-pager
```

```sh
sudo systemctl enable --now newsagent-poll.timer newsagent-digest.timer
```

## Operations

- Logs: `journalctl -u newsagent-poll -u newsagent-digest -f`
- Send a digest now: `sudo systemctl start newsagent-digest.service`
- Data: `/var/lib/newsagent/newsagent.db` (SQLite; items older than 30 days are pruned)

## Config reference

| Key | Default | Meaning |
|---|---|---|
| `timezone` | `UTC` | zone for times shown in digests |
| `max_age` | `48h` | ignore items published earlier than this |
| `hn.top_n` | `30` | number of HN top stories to consider |
| `x.max_posts_per_poll` | `25` | 5..100 |
| `x.max_posts_per_day` | `200` | hard daily cap (UTC day) on billed reads |
| `x.min_interval` | `1h` | minimum time between X reads |
| `llm.model` | `claude-opus-5-5` | Claude model for scoring and digests |
| `llm.score_effort` / `llm.digest_effort` | `low` / `medium` | Claude effort levels |
| `thresholds.urgent` / `thresholds.digest` | `9` / `6` | minimum score for a ping / a digest line |
| `thresholds.max_urgent_per_poll` | `3` | cap on pings per poll |

The digest schedule lives in `newsagent-digest.timer` (`OnCalendar=`).
