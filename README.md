<p align="center">
  <img src="assets/icon.png" alt="Vio logo" width="112" height="112">
</p>

<p align="center">
  A self-hosted media server for films, series, audiobooks, ebooks, podcasts, and manga.
</p>

<p align="center">
  <a href="https://github.com/kipsilabs/Vio/releases"><img alt="Latest GitHub release" src="https://img.shields.io/github/v/release/kipsilabs/Vio?include_prereleases&amp;sort=semver&amp;display_name=tag&amp;style=flat-square&amp;label=release"></a>
  <a href="https://github.com/kipsilabs/Vio/pkgs/container/vio"><img alt="Container image on GHCR" src="https://img.shields.io/badge/container-GHCR-2496ED?style=flat-square&amp;logo=docker&amp;logoColor=white"></a>
  <a href="https://github.com/kipsilabs/Vio/actions/workflows/ci.yml"><img alt="Continuous integration" src="https://img.shields.io/github/actions/workflow/status/kipsilabs/Vio/ci.yml?branch=main&amp;style=flat-square&amp;label=CI"></a>
  <img alt="Go 1.26" src="https://img.shields.io/badge/Go-1.26-00ADD8?style=flat-square&amp;logo=go&amp;logoColor=white">
  <img alt="React 19" src="https://img.shields.io/badge/React-19-149ECA?style=flat-square&amp;logo=react&amp;logoColor=white">
  <a href="LICENSE"><img alt="AGPL-3.0-or-later license" src="https://img.shields.io/badge/license-AGPL--3.0--or--later-555555?style=flat-square"></a>
</p>

<p align="center">
  <a href="#quick-start">Quick start</a>
  · <a href="https://siloserver.org/docs">Documentation</a>
  · <a href="docs/release-versioning.md">Builds &amp; releases</a>
  · <a href="docs/silo-to-vio-migration.md">Migrating from Silo</a>
  · <a href="CONTRIBUTING.md">Contributing</a>
</p>

---

> [!WARNING]
> Vio is pre-release. APIs, configuration, and database migrations may change
> before the first stable release. Back up your deployment before updating.

## 1.0 release scope

The [1.0 milestone](https://siloserver.org/milestone/1.0/) covers movies and
series. Audiobooks, ebooks, and Audiobookshelf compatibility keep working as
they do today and are labeled **beta**: they are outside the 1.0 support
promise and will be replaced by a consolidated Books effort with no assigned
release date. The broader capabilities below include those beta
implementations, not a promise that every media type is supported in 1.0.
Existing library data is preserved when upgrading.

## What Vio does

<table>
  <tr>
    <td width="33%" valign="top">
      <strong>Play</strong><br><br>
      Direct play when possible, remux when needed, transcode otherwise, with
      VA-API, Quick Sync, and NVENC hardware acceleration.
    </td>
    <td width="33%" valign="top">
      <strong>Organize</strong><br><br>
      One catalog for films, series, audiobooks, ebooks, podcasts, and manga,
      matched through metadata plugins such as TMDB and TVDB.
    </td>
    <td width="33%" valign="top">
      <strong>Connect</strong><br><br>
      Use the included web app, or the Jellyfin/Emby-compatible API with clients
      such as <a href="https://vidhub.okaapps.com/what-does-vidhub-do/">VidHub</a>,
      <a href="https://github.com/jarnedemeulemeester/findroid">Findroid</a>, and
      <a href="https://firecore.com/infuse">Infuse</a>. Client coverage varies.
    </td>
  </tr>
  <tr>
    <td width="33%" valign="top">
      <strong>Share</strong><br><br>
      Household profiles with their own watch state, library access, and
      parental controls.
    </td>
    <td width="33%" valign="top">
      <strong>Manage</strong><br><br>
      Libraries, users, providers, storage, search, and playback are configured
      in the admin interface, not in config files.
    </td>
    <td width="33%" valign="top">
      <strong>Scale</strong><br><br>
      Start with one integrated server; split proxy and transcode roles across
      shared PostgreSQL and Redis when you need to.
    </td>
  </tr>
</table>

## Quick start

Requires Docker Compose 2.24 or newer. The default stack runs Vio Server,
PostgreSQL with pgvector, and Redis.

```sh
git clone https://github.com/kipsilabs/Vio.git
cd Vio
cp .env.example .env
chmod 600 .env
printf '\nPOSTGRES_PASSWORD=%s\nSECRET_KEY=%s\n' \
  "$(openssl rand -hex 24)" "$(openssl rand -base64 48)" >> .env
```

Set `MEDIA_ROOT` in `.env` to the absolute path of your media, then:

```sh
docker compose up -d
```

Open <http://localhost:8090> and complete onboarding.

The published developmental image is `ghcr.io/kipsilabs/vio:dev` (override with
`VIO_IMAGE` to pin a tag or use `:latest` for releases). Server data lives under `/opt/vio` by default;
set `VIO_DATA_ROOT` in `.env` to relocate it. When running from source, the
server binary is `./vio`.

The [Docker deployment guide](docs/wiki/deployment/docker.md) covers the
`SECRET_KEY` backup requirement, storage paths, GPU acceleration, Meilisearch,
external PostgreSQL and Redis, distributed roles, PostgreSQL tuning, backups,
and updates. Migrating from Silo? Use the
[Vio migration guide](docs/silo-to-vio-migration.md).

## Builds and releases

Until the first release, default-branch images carry an ordered `build-N` tag
and a short commit SHA alongside `latest`. Build numbers order published images;
they are not release versions. [Release versioning](docs/release-versioning.md)
defines each tag and the SemVer contract.

## Documentation

- [Documentation index](docs/wiki/index.md) — user and operator guides
- [Silo to Vio migration](docs/silo-to-vio-migration.md) — Phase 1 cutover for Silo installs
- [Development guide](DEVELOPMENT.md) — source setup, builds, tests, migrations
- [Settings API](docs/settings-api.md), [Downloads API](docs/downloads-api.md), and [Apple Push Display Token](docs/notifications-push-api.md) — client contracts

## Community and contributions

Vio is an independent fork with its own builds. Please file issues and pull
requests against this repository (`kipsilabs/Vio`); upstream Silo
does not take Vio patches.

Read [CONTRIBUTING.md](CONTRIBUTING.md) before opening a pull request. Features,
API changes, migrations, and behavior changes should start as an issue.

## License and trademarks

Vio's source code is licensed under the
**GNU Affero General Public License v3.0 or later** (`AGPL-3.0-or-later`). See
[LICENSE](LICENSE).

Vio is based on Silo (© Silo Media L.L.C., AGPL-3.0-or-later). Silo is a trademark of Silo Media L.L.C.; Vio is an unofficial fork, not affiliated with or endorsed by Silo Media.

The **Vio name, logo, and wordmark belong to this fork** and are not covered
by the AGPL. Forks and redistributions may use the code but must not use the
Vio brand as their identity and must remove or replace the brand assets. The
Silo marks remain the property of Silo Media L.L.C. and are used here only
referentially. See [TRADEMARK.md](TRADEMARK.md).
