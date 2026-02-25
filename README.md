[![GitHub license](https://img.shields.io/github/license/dkorunic/e-dnevnik-bot)](https://github.com/dkorunic/e-dnevnik-bot/blob/master/LICENSE)
[![GitHub release](https://img.shields.io/github/release/dkorunic/e-dnevnik-bot)](https://github.com/dkorunic/e-dnevnik-bot/releases/latest)
[![Go Report Card](https://goreportcard.com/badge/github.com/dkorunic/e-dnevnik-bot)](https://goreportcard.com/report/github.com/dkorunic/e-dnevnik-bot)
[![goreleaser](https://github.com/dkorunic/e-dnevnik-bot/actions/workflows/goreleaser.yml/badge.svg)](https://github.com/dkorunic/e-dnevnik-bot/actions/workflows/goreleaser.yml)

![](gopher.png)

## Table of Contents

- [About](#about)
- [Privacy policy](#privacy-policy)
- [Requirements](#requirements)
- [Installation](#installation)
  - [Usage](#usage)
  - [Configuration](#configuration)
    - [User configuration](#user-configuration)
    - [Telegram configuration](#telegram-configuration)
    - [Discord configuration](#discord-configuration)
    - [Slack configuration](#slack-configuration)
    - [Mail/SMTP configuration](#mailsmtp-configuration)
    - [WhatsApp configuration](#whatsapp-configuration)
    - [Google Calendar configuration](#google-calendar-configuration)
- [HOWTO](#howto)
  - [Integration with Systemd](#integration-with-systemd)
  - [Running as a Docker container](#running-as-a-docker-container)
  - [Running using Docker Compose](#running-using-docker-compose)
    - [Step 1: Define services in a Compose file](#step-1-define-services-in-a-compose-file)
    - [Step 2: Create persistent directory and download configuration file](#step-2-create-persistent-directory-and-download-configuration-file)
    - [Step 3: How to run and stop docker compose](#step-3-how-to-run-and-stop-docker-compose)
  - [Running in Github Actions](#running-in-github-actions)
- [Star History](#star-history)

## About

e-Dnevnik bot is a self-hosted alerting system that reads from the official [CARNet e-Dnevnik](https://ocjene.skole.hr/) portal and regularly polls for new information (e.g. new grades for all subjects, newly scheduled exams, etc.).

The bot can log in as multiple AAI/AOSI users from the skole.hr domain and check for new information for all of them, either as a one-shot run or as a long-running service that polls at regular intervals (e.g. hourly). All new, previously unseen events will trigger an alert. The bot can deliver alerts through the following messaging systems and services:

- [Discord](https://discord.com/)
- [Telegram](https://telegram.org/)
- [Slack](https://slack.com/)
- [WhatsApp](https://www.whatsapp.com/)
- Regular e-mail (e.g. Gmail SMTP, etc.)
- [Google Calendar](https://calendar.google.com/) (exam events only)

Each alert can be broadcast through multiple services simultaneously, and each service can have multiple recipients. All authentication credentials remain exclusively on your PC or server.

**Important note (May 2023): e-Dnevnik bot will not be able to fetch data unless it is hosted inside Croatia, as CARNet has implemented a firewall that blocks access from non-Croatian IP addresses.** This affects all popular cloud VM and VPS providers such as Oracle Cloud, Contabo, AWS, Azure, and GitHub Actions. Self-hosting at home or in a local office is strongly recommended.

## Privacy policy

The bot uses Google's Application Programming Interface (API) Services to add school exam events to your Google Calendar.

Use of information received from Google APIs complies with the [Google API Services User Data Policy](https://developers.google.com/terms/api-services-user-data-policy), including the [Limited Use requirements](https://support.google.com/cloud/answer/9110914#explain-types&zippy=%2Ccould-you-explain-the-limited-use-requirements-from-the-google-api-services-user-data-policy).

Your Google information is used solely to provide the user-facing calendar integration feature. No data is collected, stored remotely, or tracked in any way. All authorization data (e.g. the Google API token) is stored locally alongside the application and never leaves your premises.

## Requirements

The bot requires:

- A working directory and a small amount of disk space for the database: approximately 1 MiB for ~50 grades,
- AAI/AOSI credentials belonging to the skole.hr domain for e-Dnevnik,
- One or more Discord, Telegram, Slack, WhatsApp, e-mail, or Google Calendar accounts/targets.

The bot runs on virtually any embedded device and any supported operating system, using approximately 20–25 MB of RSS memory during normal service operation.

## Installation

Download the binary from the [releases](https://github.com/dkorunic/e-dnevnik-bot/releases) page along with the [example configuration file](https://raw.githubusercontent.com/dkorunic/e-dnevnik-bot/main/.e-dnevnik.toml.example).

### Usage

```shell
NAME
  e-dnevnik-bot

FLAGS
  -v, --verbose                verbose/debug log level
  -0, --fulldebug              log every scraped event (only with verbose mode)
  -d, --daemon                 enable daemon mode (running as a service)
  -?, --help                   display help
  -t, --test                   send a test event (to check if messaging works)
  -l, --colorlogs              enable colorized console logs
      --version                display program version
      --readinglist            send reading list alerts
  -j, --jitter BOOL            enable slight (up to 10%) jitter for tick intervals (default: true)
  -f, --conffile STRING        configuration file (in TOML) (default: .e-dnevnik.toml)
  -b, --database STRING        alert database file (default: .e-dnevnik.db)
  -g, --calendartoken STRING   Google Calendar token file (default: calendar_token.json)
  -c, --cpuprofile STRING      CPU profile output file
  -m, --memprofile STRING      memory profile output file
  -i, --interval DURATION      interval between polls when in daemon mode (default: 1h0m0s)
  -p, --relevance DURATION     maximum relevance period for events (0 = unlimited) (default: 0s)
  -r, --retries UINT           number of retry attempts on error (default: 3)
```

By default, the bot runs from the current working directory and loads its [TOML](https://github.com/toml-lang/toml) configuration from `.e-dnevnik.toml`, or from the file specified with the `-f` flag.

Other flags:

- `-b`: path to the alert database file used to track seen alerts (default: `.e-dnevnik.db`); note that the `.sqlite` extension is appended automatically, so the actual file on disk will be `.e-dnevnik.db.sqlite`,
- `-d`: enables daemon/service mode, where the bot runs continuously and wakes up at regular intervals (specified with `-i`); disabled by default,
- `-f`: path to the configuration file containing usernames, passwords, and messaging service settings (in [TOML](https://github.com/toml-lang/toml) format),
- `-i`: interval between polls in daemon/service mode (minimum 1h, default 1h),
- `-r`: number of retry attempts on scraping or delivery failures (default: 3),
- `-t`: sends a test message to all configured messaging services,
- `-v`: enables verbose/debug logging for detailed insight into bot operation; disabled by default,
- `-l`: enables colorized console logging with JSON output disabled,
- `-g`: path to the Google Calendar API token file for reading and storing the OAuth2 token,
- `-p`: maximum relevance period for non-exam events, to avoid sending alerts for retroactively changed entries,
- `--version`: displays the program version,
- `-j`: enables a ±10% random jitter on the poll interval,
- `--readinglist`: enables processing and alerting on reading list events.

### Configuration

The configuration file consists of several blocks. The user block can be repeated as many times as needed. The Telegram, Discord, Slack, and e-mail blocks can each appear only once, but all can be enabled or disabled independently. Recipient lists (user IDs, chat IDs, and `to` addresses) are defined as arrays and support any number of entries. Alerts are broadcast to all enabled messaging services simultaneously.

#### User configuration

```toml
[[user]]
username = "ime.prezime@skole.hr"
password = "lozinka"
```

As many `[[user]]` blocks as needed can be specified; all users are processed in parallel.

#### Telegram configuration

```toml
[telegram]
token = "telegram_bot_token"
chatids = [ "chat_id", "chat_id2" ]
```

Steps required:

1. Create a Telegram bot by following the official [Telegram bot guide](https://core.telegram.org/bots#3-how-do-i-create-a-bot), which involves messaging BotFather and completing a few simple steps.
2. After creating the bot, message it from each Telegram account you want to receive notifications. Then retrieve the Chat IDs, typically via [https://api.telegram.org/botTOKEN/getUpdates](https://api.telegram.org/botTOKEN/getUpdates), replacing **TOKEN** with the bot token obtained in step 1.

#### Discord configuration

```toml
[discord]
token = "discord_bot_token"
userids = [ "user_id", "user_id2" ]
```

Steps required:

1. Create a Discord bot by following the [Discord bot guide](https://discordpy.readthedocs.io/en/stable/discord.html). This involves both creating the bot and inviting it to your server.
2. The only permission needed is **Send Messages**.
3. User IDs can be found by [enabling](https://www.remote.tools/remote-work/how-to-find-discord-id) **Developer Mode** in your Discord client and then messaging your bot.

#### Slack configuration

```toml
[slack]
token = "xoxb-slack_bot_token"
chatids = [ "chat_id", "chat_id2" ]
```

Steps required:

1. Create a Slack bot by following the [official Slack bot guide](https://slack.com/help/articles/115005265703-Create-a-bot-for-your-workspace).
2. The only OAuth scope required is **chat:write**.
3. To find a member's Chat ID, click their username in Slack, select **View full profile**, then **Copy member ID**. A channel ID can also be used to send messages to a group channel.

#### Mail/SMTP configuration

```toml
[mail]
server = "smtp.gmail.com"
port = "587"
username = "user.name@gmail.com"
password = "legacy_app_password"
from = "user.name@gmail.com"
subject = "Nova ocjena iz e-Dnevnika"
to = [ "user.name@gmail.com", "user2.name2@gmail.com" ]
```

Steps required:

1. For Gmail, follow the [Gmail Help Center guide](https://support.google.com/a/answer/176600?hl=en) to configure SMTP access. Other SMTP providers follow a similar, self-explanatory setup.

#### WhatsApp configuration

```toml
[whatsapp]
phonenumber = "+385XXYYYYYYY"
userids = [ "+385XXYYYYYYY@s.whatsapp.net", "XXXYYYYYYY-ZZZZZZZZZZ@s.whatsapp.net" ]
groups = [ "group1", "group2" ]
```

Steps required:

1. Open the WhatsApp mobile application (iOS, Android, etc.).
2. A phone number is required for automatic pairing via PIN. If no phone number is configured, pairing falls back to a QR code, which requires an interactive run on a desktop. During the initial sync, keep your Android/iOS WhatsApp application open and active for at least 2 minutes.
3. Entries in `userids` are either a personal JID (in the form `+385XXYYYYYYY@s.whatsapp.net`, for direct messages) or a group chat JID (in the form `XXXYYYYYYY-ZZZZZZZZZZ@s.whatsapp.net`). If you do not know a group's JID, you can specify groups by name in the `groups` field; the bot will print the group's JID in a debug message. Using `userids` directly is preferred for performance reasons.
4. The WhatsApp session is stored in `.e-dnevnik.wa.sqlite` in the working directory. When running in Docker, include this file in your persistent volume mount so that the pairing survives container restarts.

#### Google Calendar configuration

```toml
[calendar]
name = "Djeca ispiti"
```

Steps required:

1. Set up the Google Calendar API by following the [Go Quickstart guide](https://developers.google.com/calendar/api/quickstart/go#set_up_your_environment) to create a project, enable the Calendar API, and download `credentials.json` to the working directory.
2. On the **first run**, the bot must be launched interactively in a terminal — it will open a browser for OAuth2 authorization and store the resulting token in `calendar_token.json` (configurable with `-g`). Subsequent runs use the cached token automatically.
3. The `name` field specifies the target Google Calendar by name (e.g. a calendar you created called `Djeca ispiti`). Only exam events are added as calendar entries.

## HOWTO

### Integration with Systemd

For a minimal [systemd](https://systemd.io/) setup running the service as user `ubuntu` inside `/home/ubuntu/e-dnevnik`, save the following file to `/etc/systemd/system/e-dnevnik.service`:

```
# /etc/systemd/system/e-dnevnik.service
[Unit]
Description=e-Dnevnik daemon
After=network-online.target

[Service]
Type=notify
WatchdogSec=30s
WorkingDirectory=/home/ubuntu/e-dnevnik
ExecStart=/home/ubuntu/e-dnevnik/e-dnevnik-bot --daemon --verbose

Restart=always
RestartSec=2
User=ubuntu
Group=ubuntu

[Install]
WantedBy=multi-user.target
```

Then enable the service with the usual commands:

```shell
systemctl daemon-reload
systemctl enable --now e-dnevnik
systemctl status e-dnevnik
```

Note: e-dnevnik-bot supports the systemd [sd_notify](https://www.freedesktop.org/software/systemd/man/latest/sd_notify.html), `Type=notify`, and `WatchdogSec=` features from version **0.13.0** onwards. Earlier versions are only compatible with `Type=simple`.

### Running as a Docker container

Up-to-date images for Linux amd64/arm64/arm are available on [Docker Hub](https://hub.docker.com/r/dkorunic/e-dnevnik-bot). To set this up, create a persistent directory named `ednevnik` in your working directory, place the configuration file there, and mount it into the container:

```shell
cd some/workdir
mkdir ednevnik

curl https://raw.githubusercontent.com/dkorunic/e-dnevnik-bot/main/.e-dnevnik.toml.example \
    --output ednevnik/.e-dnevnik.toml
editor ednevnik/.e-dnevnik.toml

docker pull dkorunic/e-dnevnik-bot

docker run --detach \
    --volume "$(pwd)/ednevnik:/ednevnik" \
    --restart unless-stopped \
    dkorunic/e-dnevnik-bot \
    --daemon \
    --verbose \
    --database /ednevnik/.e-dnevnik.db \
    --conffile /ednevnik/.e-dnevnik.toml
```

### Running using Docker Compose

To use `docker compose`, first ensure [Docker Compose is installed](https://docs.docker.com/compose/install/).

#### Step 1: Define services in a Compose file

Create a `docker-compose.yml` file in your project directory (e.g. `~/docker-compose/ednevnik`):

```bash
user@server:~/docker-compose$ mkdir ednevnik
user@server:~/docker-compose$ cd ednevnik
user@server:~/docker-compose$ editor docker-compose.yml
```

Paste the following into `docker-compose.yml`:

```yaml
version: "3"
# More info at https://github.com/dkorunic/e-dnevnik-bot
services:
  ednevnik:
    container_name: e-dnevnik
    image: dkorunic/e-dnevnik-bot:latest
    command:
      - "--daemon"
      - "--database=/ednevnik/.e-dnevnik.db"
      - "--conffile=/ednevnik/.e-dnevnik.toml"
    # Volumes store your data between container upgrades
    volumes:
      - ./ednevnik:/ednevnik
    restart: unless-stopped
```

#### Step 2: Create persistent directory and download configuration file

Inside the project directory, create a directory called `ednevnik` for persistent storage, then download and edit the configuration file:

```bash
user@server:~/docker-compose/ednevnik$ mkdir ednevnik

user@server:~/docker-compose/ednevnik$ curl https://raw.githubusercontent.com/dkorunic/e-dnevnik-bot/main/.e-dnevnik.toml.example \
    --output ednevnik/.e-dnevnik.toml

user@server:~/docker-compose/ednevnik$ editor ednevnik/.e-dnevnik.toml
```

#### Step 3: How to run and stop docker compose

From the project directory where `docker-compose.yml` is located, start the service with:

```bash
user@server:~/docker-compose/ednevnik$ docker compose up -d
```

The `-d` / `--detach` flag runs the containers in the background.

To stop the service, run:

```bash
user@server:~/docker-compose/ednevnik$ docker compose down
```

### Running in Github Actions

> **Warning:** CARNet has implemented a firewall that blocks access to e-Dnevnik from non-Croatian IP addresses. GitHub Actions runners use IP addresses outside Croatia, so this integration will **fail to scrape data**. It is kept here for reference only. For reliable operation, self-host the bot on hardware located in Croatia.

This integration was originally created by Luka Kladaric [@allixsenos](https://twitter.com/allixsenos) — thanks, Luka! The original Gist is [here](https://gist.github.com/allixsenos/f12977de767f32450f435ec2f33b93f0); a copy is reproduced below:

```yaml
name: e-imenik run

# Controls when the action will run.
on:
  # Allows you to run this workflow manually from the Actions tab
  workflow_dispatch:
  # Run on push to any branch for testing
  push:
  # Run every 6 hours
  schedule:
    - cron: "0 */6 * * *"

jobs:
  build:
    runs-on: ubuntu-latest

    steps:
      - uses: actions/checkout@v2

      - name: Render the config file
        run: |
          cat > .e-dnevnik.toml <<EOF
          [[user]]
          username = "$SKOLE_USERNAME"
          password = "$SKOLE_PASSWORD"

          [telegram]
          token = "$TELEGRAM_TOKEN"
          chatids = [ "$TELEGRAM_CHATID" ]
          EOF
        env:
          SKOLE_USERNAME: ${{ secrets.SKOLE_USERNAME }}
          SKOLE_PASSWORD: ${{ secrets.SKOLE_PASSWORD }}
          TELEGRAM_TOKEN: ${{ secrets.TELEGRAM_TOKEN }}
          TELEGRAM_CHATID: ${{ secrets.TELEGRAM_CHATID }}

      - name: Run dkorunic/e-dnevnik-bot
        run: |
          docker run -t \
            --volume "$(pwd):/ednevnik" \
            --user $(id -u):$(id -g) \
            dkorunic/e-dnevnik-bot \
            --verbose \
            --database /ednevnik/.e-dnevnik.db \
            --conffile /ednevnik/.e-dnevnik.toml

      - name: Delete the config file so it doesn't get committed back :)
        run: rm .e-dnevnik.toml

      - name: Commit the DB
        uses: stefanzweifel/git-auto-commit-action@v4
        with:
          commit_author: GitHub Actions <actions@github.com>
```

This GitHub Actions workflow runs every 6 hours. On each run it checks out the repository (which includes the database), renders the configuration file from secrets, runs the bot, and commits the updated database back to the repository, providing persistence across runs.

## Star History

[![Star History Chart](https://api.star-history.com/svg?repos=dkorunic/e-dnevnik-bot&type=Date)](https://star-history.com/#dkorunic/e-dnevnik-bot&Date)
