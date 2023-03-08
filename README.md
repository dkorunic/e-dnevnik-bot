# e-dnevnik-bot

[![GitHub license](https://img.shields.io/github/license/dkorunic/e-dnevnik-bot)](https://github.com/dkorunic/e-dnevnik-bot/blob/master/LICENSE)
[![GitHub release](https://img.shields.io/github/release/dkorunic/e-dnevnik-bot)](https://github.com/dkorunic/e-dnevnik-bot/releases/latest)
[![Go Report Card](https://goreportcard.com/badge/github.com/dkorunic/e-dnevnik-bot)](https://goreportcard.com/report/github.com/dkorunic/e-dnevnik-bot)
[![goreleaser](https://github.com/dkorunic/e-dnevnik-bot/actions/workflows/goreleaser.yml/badge.svg)](https://github.com/dkorunic/e-dnevnik-bot/actions/workflows/goreleaser.yml)

![](gopher.png)

## About / Opće informacije

e-Dnevnik bot is a self-hosting alerting system which reads from the official [CARNet e-Dnevnik](https://ocjene.skole.hr/) which regularly polls for new information (ie. new grades for all lecture subjects, new scheduled exams, etc).

Bot is able to login as multiple AAI/AOSI users from skole.hr and check for new information for all of them either in a single-run or as a service, doing polls in regular intervals (ie. hourly). All new alerts (previously not seen) will be alerted on. This bot is able to send alerts through the following message systems and/or services:

- [Discord](https://discord.com/)
- [Telegram](https://telegram.org/)
- [Slack](https://slack.com/)
- regular e-mail (ie. Gmail SMTP, etc.)

Each alert can be broadcasted through multiple services and each of those services can have multiple recipients. All and any authentication information remains on your PC and/or server alone.

--

e-Dnevnik je bot i obavjesni sustav koji se izvršava u potpunosti kod krajnjeg korisnika, a zamišljen je kao nadogradnja na [CARNet e-Dnevnik](https://ocjene.skole.hr/). Korisnik pri tome više ne mora redovno otvarati e-Dnevnik u potrazi za novim informacijama. Bot može jednokratno ili u redovnim intervalima dohvaćati nove informacije o predmetima (nove ocjene i novi zakazani ispiti).

Bot se može autenticirati kao različiti AAI/AOSI korisnici iz skole.hr domene, te može provjeravati informacije bilo jednokratno, bilo kao servis koji povlači informacije u redovnim intervalima. Bot će poslati sve obavijesti za sve nove događaje koji do sad nisu prikazani, a može ih slati kroz različite sustave slanja poruka:

- [Discord](https://discord.com/)
- [Telegram](https://telegram.org/)
- [Slack](https://slack.com/)
- standardni e-mail (npr. Gmail SMTP)

Svaka ta poruka će se proslijediti kroz jedan ili više servisa i svaki navedeni servis može imati konfiguranog jednog ili više primatelja. Autentikacijski podaci za sve navedeno ostaju isključivo lokalno i ne napuštaju vaše računalo i/ili server.

## Requirements / Zahtjevi za rad

Bot needs:

- a folder to run from as well as some (very small) amount of disk space for the database: 1 MiB of disk space for ~50 grades,
- AAI/AOSI logins belonging to skole.hr domain for e-Dnevnik,
- one or more Discord, Telegram, Slack or e-mail messaging accounts.

Bot will be most likely able to run on any embedded device and on any supported operating system, as it uses ~20-25MB RSS during regular operation as a service.

--

Bot za rad treba:

- direktorij iz kojeg će raditi te koji će sadržavati bazu podataka za poslane poruke, cca 1 MiB prostora za cca 50ak ocjena,
- AAI/AOSI korisničke podatke iz skole.hr domene za pristup e-Dnevniku,
- jedan ili više Discord, Telegram, Slack ili e-mail korisničkih računa.

Bot bi trebao moći funkcionirati na bilo kakvom embedded računalu (Raspberry Pi itd.) kao i bilo kakvom podržanom operativnom sustavu, te koristi cca 20-25MB radne memorije tijekom rada.

## Installation / Instalacija

Just download the binary from the [releases](https://github.com/dkorunic/e-dnevnik-bot/releases) page as well as the [configuration file](https://raw.githubusercontent.com/dkorunic/e-dnevnik-bot/main/.e-dnevnik.toml.example).

--

Za instalaciju dovoljno je skinuti izvršnu datoteku sa [releases](https://github.com/dkorunic/e-dnevnik-bot/releases) stranice te [konfiguracijsku datoteku](https://raw.githubusercontent.com/dkorunic/e-dnevnik-bot/main/.e-dnevnik.toml.example).

### Usage / Upute za upotrebu

```shell
Usage: e-dnevnik-bot [-?dltv] [-b value] [-c value] [-f value] [-i value] [-m value] [-r value] [parameters ...]
 -?, --help       display help
 -b, --database=value
                  alert database file [.e-dnevnik.db]
 -c, --cpuprofile=value
                  CPU profile output file
 -d, --daemon     enable daemon mode (running as a service)
 -f, --conffile=value
                  configuration file (in TOML) [.e-dnevnik.toml]
 -i, --interval=value
                  interval between polls when in daemon mode [1h]
 -l, --colorlogs  enable colorized console logs
 -m, --memprofile=value
                  memory profile output file
 -r, --retries=value
                  default retry attempts on error [3]
 -t, --test       send a test event (to check if messaging works)
 -v, --verbose    enable verbose/debug log level
```

Typically bot will run from current working directory and attempt to load [TOML](https://github.com/toml-lang/toml) configuration from `.e-dnevnik.toml` file or the file specified with `-f` flag.

Other flags are:

- `-b`: alert database file path used to mark seen alerts (default is `.e-dnevnik.db`),
- `-d`: enable daemon mode aka service mode where bot works continously, waking up on regular intervals (specified with `-i`) and by default this is disabled,
- `-f`: configuration file path to configure usernames, passwords and various messaging services (in [TOML](https://github.com/toml-lang/toml) format),
- `-i`: interval between polls when in daemon/service mode (at minimum 1h, default 1h),
- `-r`: retries between unsuccessful attempts to scrape and/or send alerts (default 3),
- `-t`: sends a test message to all configured messaging services,
- `-v`: enables verbose/debug messages for more insight into bot operation and by default this is disabled,
- `-l`: enables colorized console logging with JSON output disabled.

--

Bot se koristi iz tekućeg direktorija u kojem se nalazi i izvršna datoteka i pokušati će učitati datoteku `.e-dnevnik.toml` koja je u [TOML](https://github.com/toml-lang/toml) sintaksi, odnosno učitati će datoteku specificiranu kroz `-f` parametar.

Ostali parametri su:

- `-b`: staza do baze poslanih obavijesti (standardno je to `.e-dnevnik.db` iz tekućeg direktorija),
- `-d`: omogućuje servisni rad gdje bot radi kontinuirano i budi se u regularnim intervalima (koje odabiremo sa `-i` parametrom) te je ovakav način rada standardno ugašen,
- `-f`: staza do konfiguracijske datoteke koja sadrži korisnička imena, lozinke i ostalu konfiguraciju za servise slanja poruka odnosno e-maila (u [TOML](https://github.com/toml-lang/toml) sintaksi),
- `-i`: interval između buđenja bota (minimalno 1h, standardno 1h),
- `-r`: broj pokušaja kod neuspjeha prilikom dohvata ocjena i/ili slanja poruka odnosno e-mailova,
- `-t`: služi za slanje testne poruke na sve konfigurirane servise slanja poruka odnosno e-maila,
- `-v`: omogućuje prikaz više informacija o radu servisa, te je standardno ova opcija ugašena,
- `-l`: omogućuje prikaz na konzolu sa obojenim porukama i gasi JSON oblik ispisa.

### Configuration / Konfiguracija

Configuration has several blocks. User configuration can be repeated as many times as needed, while Telegram, Discord, Slack and e-mail configuration blocks can be appear only once, but they can be all enabled and disabled as needed. Targets (User IDs, Chat IDs and To) are defined as arrays and permit as many receivers as needed. Alerts are broadcasted to all of chat services or e-mail service at once.

--

Konfiguracija ima nekoliko blokova. Konfiguracija za korisnika se može ponavljati nekoliko puta za različite korisnike iz @skole.hr domene. Konfiguracije za Telegram, Discord, Slack i e-mail se mogu odnosno smiju pojaviti samo jednom ali mogu biti omogućene sve po potrebi. Odredišta (User ID, Chat ID, To) su sva definirana kao vektori i dozvoljavaju unošenje koliko je god potrebno odredišta koliko treba. Sve obavijesti se šalju istovremeno na sve servise odnosno e-mail.

#### User configuration

```toml
[[user]]
username = "ime.prezime@skole.hr"
password = "lozinka"
```

It is possible to specify as many of user blocks as needed and they will all get processed in parallel.

--

Moguće je definirati koliko je god potrebno korisnika i podaci za sve će se dohvaćati i obrađivati istovremeno.

#### Telegram configuration

```toml
[telegram]
token = "telegram_bot_token"
chatids = [ "chat_id", "chat_id2" ]
```

Steps required:

1. Create a Telegram bot by following the official [Telegram bot HOWTO](https://core.telegram.org/bots#3-how-do-i-create-a-bot), which amounts to messaging BotFather and doing a few simple steps.
2. When you create a bot, you will need to message it directly from each Telegram account you plan to configure for the bot to message and find Chat IDs, typically by using [https://api.telegram.org/botTOKEN/getUpdates](https://api.telegram.org/botTOKEN/getUpdates) and replacing **TOKEN** with the Bot Token you got from step 1.

--

Potrebni koraci:

1. Stvara se Telegram bot prateći [službene upute](https://core.telegram.org/bots#3-how-do-i-create-a-bot), što se svodi na slanje poruke BotFather korisniku i praćenje dobivenih uputa.
2. Kada se dovrši prethodni korak i bot je stvoren, treba mu poslati poruku sa svakog Telegram accounta kojeg želimo dodati kao korisnika. Chat ID se zatim može pronaći koristeći [https://api.telegram.org/botTOKEN/getUpdates](https://api.telegram.org/botTOKEN/getUpdates) link u kojem ste zamijenili riječ **TOKEN** sa Bot Token zapisom iz koraka 1.

#### Discord configuration

```toml
[discord]
token = "discord_bot_token"
userids = [ "user_id", "user_id2" ]
```

Steps required:

1. Create a Discord bot by following the [Discord bot HOWTO](https://discordpy.readthedocs.io/en/stable/discord.html). This step requires both creating a bot and inviting it to your server.
2. Permissions neded should be set only to **Send Messages** and nothing else,
3. You can find User IDs by [enabling](https://www.remote.tools/remote-work/how-to-find-discord-id) **Developer Mode** in your Discord client after messaging your bot.

--

Potrebni koraci:

1. Stvara se Discord bot prateći [neslužbene upute](https://discordpy.readthedocs.io/en/stable/discord.html). Ovaj korak podrazumijeva i stvaranje bota i pozivanje njega na vlastiti server.
2. Potrebne dozvole su isključivo one za slanje poruka odnosno **Send Messages**.
3. Moguće je pronaci User ID tako da se upali [način razvijanja](https://www.remote.tools/remote-work/how-to-find-discord-id) odnosno **Developer Mode** u Discord klijentu i pogleda u chatu koji se otvori nakon slanja poruke botu.

#### Slack configuration

```toml
[slack]
token = "xoxb-slack_bot_token"
chatids = [ "chat_id", "chat_id2" ]
```

Steps required:

1. Create a Slack bot by following the [official Slack bot HOWTO](https://slack.com/help/articles/115005265703-Create-a-bot-for-your-workspace).
2. Permissions that are needed are only **chat:write**.
3. Chat IDs can be copied from Slack user interface, just click either on a desired username, then View full profile, then **Copy member ID**. Channel ID can be also used instead, when sending a group message.

--

Potrebni koraci:

1. Stvara se Slack bot prateći [službene upute](https://slack.com/help/articles/115005265703-Create-a-bot-for-your-workspace).
2. Potrebne dozvole su isključivo **chat:write**.
3. Chat ID se može naći iz Slack klijenta, dovoljno je kliknuti na željenog korisnika, zatim View full profile te onda **Copy member ID**. Moguće je koristiti i Channel ID ako Slack bot treba slati grupne poruke.

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

1. Gmail SMTP configuration can be set up by following Gmail [Help Center answer](https://support.google.com/a/answer/176600?hl=en). Other SMTP services follow the similar, self-explanatory configuration.

--

Potrebni koraci:

1. Gmail SMTP konfiguraciju je moguće složiti koristeći odgovor sa [Google centra](https://support.google.com/a/answer/176600?hl=en) za pomoć. Svi ostali SMTP servisi se slično konfiguriraju.

## HOWTO

### Integration with Systemd

To have minimal [systemd](https://systemd.io/) configuration with the service running as user `ubuntu` inside `/home/ubuntu/e-dnevnik` directory, the following file should be saved to `/etc/systemd/system/e-dnevnik.service`:

```
# /etc/systemd/system/e-dnevnik.service
[Unit]
Description=e-Dnevnik daemon
After=network-online.target

[Service]
WorkingDirectory=/home/ubuntu/e-dnevnik
ExecStart=/home/ubuntu/e-dnevnik/e-dnevnik-bot --daemon --verbose

Restart=always
RestartSec=2
User=ubuntu
Group=ubuntu

[Install]
WantedBy=multi-user.target
```

Steps to enable would be as usual:

```shell
systemctl daemon-reload
systemctl enable --now e-dnevnik
systemctl status e-dnevnik
```

### Running as a Docker container

We have up to date [Docker Hub](https://hub.docker.com/r/dkorunic/e-dnevnik-bot) builds that can be used to run bot as Linux amd64/arm64/arm containers. To set this up, we need a persistent directory named `ednevnik` on host in a local folder containing configuration file `.e-dnevnik-toml`. That same directory will also store persistent alerts database named `.e-dnevnik.db` as well, and we will do a classic volume mount from host to container:

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

In order to use `docker compose` you need to have [Docker Compose installed](https://docs.docker.com/compose/install/).

#### Step 1: Define services in a Compose file

Create a ednevnik file called `docker-compose.yml` in your project directory (ie. `~/docker-compose/ednevnik`):

```bash
user@server:~/docker-compose$ mkdir ednevnik
user@server:~/docker-compose$ cd ednevnik
user@server:~/docker-compose$ editor docker-compose.yml
```

and paste the following to the `docker-compose.yml` file:

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

In your project directory create a directory called `ednevnik` which will be persistent directory and follow the instructions from [Running as a Docker container](#configuration--konfiguracija) in order to download and configure `.e-dnevnik.toml` configuration file.

```bash
user@server:~/docker-compose/ednevnik$ mkdir ednevnik

user@server:~/docker-compose/ednevnik$ curl https://raw.githubusercontent.com/dkorunic/e-dnevnik-bot/main/.e-dnevnik.toml.example \
    --output ednevnik/.e-dnevnik.toml

user@server:~/docker-compose/ednevnik$ editor ednevnik/.e-dnevnik.toml
```

#### Step 3: How to run and stop docker compose

In project directory where `docker-compose.yml` is located run docker compose command as follows:

```bash
user@server:~/docker-compose/ednevnik$ docker compose up -d
```

Option `-d` or `--detach` means detached mode and will run containers in the background

In order to stop docker run following command:

```bash
user@server:~/docker-compose/ednevnik$ docker compose down
```

### Running in Github Actions

This great and simple integration has been created by Luka Kladaric [@allixsenos](https://twitter.com/allixsenos), thanks Luka! Link to his original Gist is [here](https://gist.github.com/allixsenos/f12977de767f32450f435ec2f33b93f0) and a copy is below:

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

This Github Actions recipe runs every 6 hours and upon each run will checkout the database, render the configuration file, run the bot and commit the database back, taking care of the persistency.
