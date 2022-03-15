# e-dnevnik-bot

[![GitHub license](https://img.shields.io/github/license/dkorunic/e-dnevnik-bot)](https://github.com/dkorunic/e-dnevnik-bot/blob/master/LICENSE)
[![GitHub release](https://img.shields.io/github/release/dkorunic/e-dnevnik-bot)](https://github.com/dkorunic/e-dnevnik-bot/releases/latest)

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

Just download the binary from the [releases](releases) page.

--

Za instalaciju dovoljno je skinuti izvršnu datoteku sa [releases](releases) stranice.

### Usage

```shell
Usage: e-dnevnik-bot [-?dv] [-b value] [-f value] [-i value] [parameters ...]
 -?, --help     display help
 -b, --database=value
                alert database file [.e-dnevnik.db]
 -d, --daemon   enable daemon mode (running as a service)
 -f, --conffile=value
                configuration file (in TOML) [.e-dnevnik.toml]
 -i, --interval=value
                interval between polls when in daemon mode [1h]
 -v, --verbose  enable verbose/debug log level
```

Typically bot will run from current working directory and attempt to load [TOML](https://github.com/toml-lang/toml) configuration from `.e-dnevnik.toml` file or the file specified with `-f` flag.
It is possible to enable debug mode with `-v` flag that increases verbosity level, showing some of internal operation details. The bot without any options runs in a single-run mode, but it is also possible to make it run as a service with `-d` flag. When bot runs as a service it will periodically wake up and fetch new grades/exams and that tick value is possible to change with `-i` option, typically specified as time duration (ie. with h, m or s suffixes). Finally it is also possible to change the database name and path with `-b` flag.

--

### Configuration / Konfiguracija
