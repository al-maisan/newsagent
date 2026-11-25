# newsagent

Watches tech news sites and your X home timeline, and keeps you up to date on Telegram.

- **Sources:** Hacker News top stories, any RSS/Atom feed, and X, either through the official API (your mentions, the posts of accounts you pick, your home timeline) or, much cheaper, through twitterapi.io search (posts and reposts of accounts you pick; about $2/month for ~10 accounts; an unofficial service, not licensed by X).
- **Ranking:** Claude rates every new item 0–10 against your interests (set in the config). It runs through the Claude Code CLI (`claude -p`) with your Claude subscription, so there are no API credit costs. Usage counts against your plan's limits.
- **Urgent pings:** items scoring 9 or higher are sent right away, at most 3 per poll, with repeats of the same story skipped.
- **Digests:** twice a day, the items scoring 6 or higher are grouped into short themed sections. An optional hourly mode adds short updates in between (see the table below). Each item appears in one digest only: a daily digest carries what no hourly update showed yet, and vice versa. Each digest is also told what went out in the last 24 hours and leaves out items that only repeat those stories.
- **X priorities:** every mention of you is pinged right away. Every post from the accounts in `x.accounts` is in the digest, and it's pinged when it scores 9 or higher. This is the official-API version of X's notification bell.
- **Alarms:** a ⚠️ Telegram message when something stops newsagent: the Claude plan's usage limit (with the reset time), X credits used up (402), X access denied (401, or 403 on your own endpoints), and the daily X read cap reached. While a problem lasts it is repeated every 6 hours (the daily cap once per day); it ends when things work again.

## How it works

| Command | Run by | What it does |
|---|---|---|
| `newsagent poll` | `newsagent-poll.timer` (every 15 min, at :07/:22/:37/:52) | fetch sources → store new items in SQLite → score with Claude → send urgent pings |
| `newsagent digest` | `newsagent-digest.timer` (08:00, 18:00 Europe/Istanbul) | group notable items that no digest showed yet into a digest → send |
| `newsagent hourly` | `newsagent-hourly.timer` (optional; 09:00–23:00 Europe/Istanbul, not 18:00) | short update with notable items that were in no digest yet → send; nothing is sent if there is nothing new |

- Links in messages always come from the fetched data, never from model output. Feed text is treated as untrusted data.
- newsagent refuses to connect to loopback, private, CGNAT (Tailscale), link-local or multicast addresses, so a compromised feed can't point it at internal services. Exceptions: feeds whose URL is an IP address, and feeds marked `"private": true` (their DNS is then trusted). The check runs on the address actually connected to, so DNS rebinding can't get around it. Redirects from https to http are refused too. Plain `http://` feeds work but log a warning at every run.
- The CLI runs with every tool, MCP server and settings file disabled, and with a clean environment (so no `ANTHROPIC_API_KEY` can switch it to API billing).
- When the plan's usage limit is hit, items stay unscored and are retried on later polls for up to 48 hours. Other temporary failures (CLI crash, overload, timeout) count against 24 tries (about 6 hours), so one batch that keeps failing can't block newer items.
- An item is marked as sent only after Telegram accepts it, so failed sends are retried on the next run. A message Telegram rejects for its content (bad HTML, too long) is resent once as plain text; if that fails too, it is logged and skipped, so it cannot block the messages behind it. Digest items in a skipped part stay pending for the next digest. Items the digest leaves out stay pending for the next digest (within 36 hours).
- X bills per post read, so reads are capped per request and per UTC day (`x.max_posts_per_poll`, `x.max_posts_per_day`). X also bills each user object it returns, so authors' usernames are looked up once and cached for 30 days instead of being requested with every read. These lookups ($0.010 per new author) are not counted by `x.max_posts_per_day`; they are high only while the cache fills (at most one per post read, so up to ~$2 on a 200-post day) and small after that. Set a spend cap in the X Developer Console as a hard limit. The order is mentions, then accounts, then home timeline, so the daily budget goes to the important feeds first. Each feed only reads posts newer than its last read. If more posts arrive than the cap allows, the excess is skipped.

## Build and test

```sh
make test && make build
```

## Setup

See [docs/setup.md](docs/setup.md).
