# Tommy — Implementation Plan (forward-looking)

Waves 0–8 are built. This document plans what comes next.

- **What was built and why:** `docs/archive/history.md`
- **The interfaces as built (authoritative):** `docs/contracts.md`
- **What the work taught us:** `docs/lessons.md`
- **The original brief:** `docs/plan.md`
- **What each plugin and provider is for:** `docs/catalogue.md`

## 0. Keeping this document current

**This plan is a working document, not a record.** It describes only what is
still to do.

- When a wave finishes, its section is **deleted from here** and appended to
  `docs/archive/history.md` in the past tense. The full ritual is in `CLAUDE.md`
  → *Finishing a wave*; it is required, not optional.
- When something learned mid-wave changes a later wave, **edit that wave now**,
  while the reason is still known. A dependency discovered, an ordering
  constraint, a task that turned out to be unnecessary — all of it belongs here
  rather than in someone's memory.
- The status table below is the single source of truth for what exists. Keep it
  accurate before anything else.

If you are reading a wave section, it has not been built. If you cannot find a
wave you expected, look in the history.

## 1. Where things stand

| Plugin | Providers | State |
|---|---|---|
| `mail` | mailjet, sendgrid, resend, smtp | done |
| `sms` | twilio | done |
| `files` | ftp, sftp, tftp, nfs | done |
| `chat` | slack, msteams | done |
| `hl7` | mllp | done |
| `snmp` | trap | done |
| `push` | fcm, apns | done |
| `as2` | http | done |

Every plugin has a `tommy <plugin>` subcommand, and every provider option worth
setting has a flag. Every plugin and provider carries user-facing documentation
— what it is, what it is for, and commands that have been run — indexed in
`docs/catalogue.md` and required from here on by `CLAUDE.md` rule 12.

Everything through wave 8·1 is merged to `main`; the six-deep review stack that
carried waves 6a–8·1 is gone, and a new wave now branches off a clean trunk.
**Start each wave on its own branch**, named for what it builds, so a wave stays
a reviewable unit — and merge it before starting the next, because a wave
branched off an unreviewed tip inherits every diff below it.

What comes next is a change of emphasis. Waves 0–8 built breadth: eight plugins
and sixteen providers, each capturing one more thing. Waves 9–12 build the
*surface* instead — how a person and a program reach what tommy captured — and
only wave 11 adds a provider. The protocol backlog is still there, renumbered,
behind them.

**Waves 9 through 12 are built**, on `feat/event-page`, `feat/openapi-spec`,
`feat/plugin-openapi`, `feat/resend-provider` and `feat/website`:
every event has a page at `/ui/events/{id}`, every API representation of an
event carries its `url`, an ingress response names what it captured in
`X-Tommy-Event-URL`, and the events API and every plugin API have generated
OpenAPI 3.1 descriptions that CI holds to the code, `mail` has a fourth
provider, and the documentation is published as a site generated from the
repository itself.

## 2. The scoping rule

It does most of the sorting, so it comes first.

Tommy captures and displays what an application *sent*, and answers with whatever
the protocol requires so the client proceeds. It does not simulate scenarios,
drive inbound traffic, or make policy decisions.

A protocol fits when its reply is **mechanical** — derivable from the request: an
HL7 `ACK`, an AS2 MDN receipt, Slack's `ok`, SMTP's `250`. It fits badly when the
reply encodes a **decision** somebody has to configure (approve or decline this
card payment; accept or reject this login), because that is scenario machinery by
another name.

Explicitly out of scope, and the plugin interface must not preclude them:
inbound traffic of any kind — webhooks and callbacks (Stripe events, Twilio
`StatusCallback`, SendGrid event webhooks, Slack interactivity and Events API,
async AS2 MDN) — which would need outbound HTTP and a scenario definition format.

## 3. How to run a wave

The pattern that worked for waves 0–8, in short. `docs/lessons.md` has the
reasoning; `CLAUDE.md` has the rules.

1. **Branch first.** One branch per wave, named for what it builds
   (`feat/hl7-plugin`). A wave is a reviewable unit; two waves on one branch stop
   being one.
2. **Split by directory.** One agent per task, exclusive ownership, and name what
   it must not touch. Disjoint paths are what makes parallelism safe.
3. **A core task blocks its providers.** Plugin cores define the canonical model
   several providers code against, so they run alone and land first.
4. **Stagger anything that touches `go.mod`.** At most one dependency-adding agent
   at a time, and its dependencies land *with* the code that imports them.
5. **Subagents run no git commands.** The coordinator commits in coherent chunks.
6. **Point each agent at `docs/contracts.md` plus a worked example** — the closest
   existing provider of the same shape.
7. **Require live-documentation verification** for anything imitating a third-party
   API, and a **real client over a socket** for anything speaking a wire protocol.
8. **Verify reports independently**, re-run the gate, and clean up stray servers.
9. **Finish by updating the documents** — §0. The wave is not done until the plan,
   the history, the contracts and the lessons match the code.
10. **Push the branch and open the pull request**, based on whatever the wave was
    branched from: `main` for a wave off trunk, the predecessor branch for a wave
    stacked on an unmerged one. `CLAUDE.md` → *Finishing a wave* step 9 has the
    mechanics, including rebasing the rest of the stack when a base moves.

Model guidance: contract-defining and subtle-parsing work to the stronger model;
well-specified translation against a fixed contract to the cheaper one.

## 4. The surface work is finished; distribution is not

Waves 9 through 12 are built and are in `docs/archive/history.md`. What is left
is wave 13 — a licence, a published image and a first release — and then the
protocol backlog. Nothing is now blocked on the repository owner: **GitHub Pages
is enabled** with the source set to *GitHub Actions*, so the site deploys from
`main`, and the **Docker Hub credentials are configured** as repository secrets,
so wave 13·3 and 13·4 can push the moment they are written.

- Each of those waves left a *keep it true* half — a generated artifact plus a
  test that fails when it stops matching the code. The OpenAPI descriptions, the
  site's coverage test and rule 12's per-component READMEs are the three places
  where documentation is checked rather than remembered, and wave 13·6 adds a
  fourth family for the Docker surface: the image's ports against the provider
  listing, the shipped config against the checked-in one, and the documented
  `docker run` executed by CI. A wave that adds a plugin or provider already
  fails the build if it ships without a README; after wave 13 it also fails if
  it moves a listener port and leaves the image behind. Treat the four as one
  family, and when a new surface is added, add its check in the same wave.

---

## Wave 13 — distribution and first release

Everything tommy does works when you build it from source. Nobody outside this
repository can *use* it: there is no licence saying they may, and no way to run
it that does not begin with a Go toolchain. This wave closes both, and it is the
last one before a `v0.1.0` tag is worth cutting.

Branch: `feat/distribution`. Seven tasks. 13·0 is a core prerequisite that two
later tasks depend on and so lands first and alone; 13·1 is independent of
everything and can run beside it. 13·2 must land before 13·3 can push anything,
13·4 describes what 13·3 published, 13·5 renders what 13·2 wrote, and 13·6 is
what stops all of it going stale — write it in this wave, not the next one.

### 13·0 — the provider listing learns its listener ports

Owns: `core/plugin/{plugin,registry,snippet}.go`, `cmd/providers.go`, the
listener providers' `Addr` reporting.

This is a small core change that exists because three later tasks each need the
same fact — *which ports does a default tommy listen on* — and none of them can
get it today.

`ProviderInfo.Addr` already exists, but `cmd/providers.go` fills it only when
the configuration explicitly names a port; with no config, `pc.Port` is 0, the
provider falls back to its own package-level `DefaultPort`, and the listing
never learns it. That is why `tommy providers --json` reports `endpoints: []`
and no address at all for `smtp`, `ftp`, `sftp`, `tftp`, `nfs`, `mllp` and
`trap`. The information exists in seven `DefaultPort` constants and nowhere a
program can reach.

- Give the listing a provider's **configured-or-default** port without binding
  anything — the provider resolves `pc.Int("port", DefaultPort)` at registration
  already, so this is reporting a value it holds, not a new decision. Keep
  `AddressableProvider.Addr` as the authority for what a *running* listener
  actually bound (ephemeral ports still need it); the new field is what
  `tommy providers` and the site can show when nothing is running.
- Do not bind a port to find out. Rule: `tommy providers` starts no listeners,
  and the tests for this must not either — the project's own convention forbids
  binding 1025/2121/2222 in a test.
- **Why it is worth a core change rather than a hand-written table**: after this,
  the Docker `EXPOSE` list, the compose file, the port table on the website and
  the Docker Hub page are all *derivable*. A new listener provider then shows up
  in every one of them, and 13·6's test fails if the image did not follow. A
  hand-maintained list would need one line added per provider by someone who
  remembered — which is the failure this wave is supposed to end.

### 13·1 — MIT licence

Owns: `LICENSE`, `README.md` (one section), `.goreleaser.yaml` (`archives.files`).

- `LICENSE` at the repository root, the unmodified MIT text, `Copyright (c)
  2026 Dmitry Petrov`. **Confirm the copyright line with the repository owner
  before committing** — it is the one string in this wave that cannot be
  inferred from the code.
- MIT requires the notice to travel with every copy, which makes this a
  packaging change and not only a file. The release archives currently ship
  `files: [only-the-binary*]` — a deliberate "binary only" idiom inherited from
  the kleiner scaffold — so `LICENSE` has to be added there explicitly, and the
  Docker image built in 13·2 has to carry it too (`/LICENSE`, plus the
  `org.opencontainers.image.licenses=MIT` label).
- `README.md` gets a short `## License` section pointing at the file. The
  website already renders the README, so that is the whole of the site's
  obligation — do **not** add a licence page that restates it (rule 14).
- Do not add a licence header to every source file. One `LICENSE` and the
  `go.mod` module path are what the Go ecosystem reads; per-file headers are
  noise a linter will eventually have to police.

### 13·2 — the Docker image

Owns: `Dockerfile`, `.dockerignore`, `docker-compose.yml`, `docs/docker.md`,
`.github/workflows/ci.yml` (one new job).

A single-binary image, multi-architecture (`linux/amd64`, `linux/arm64`), built
from the binaries GoReleaser already produces rather than from a second `go
build` inside the image.

- **Base image: `gcr.io/distroless/static:nonroot`.** It brings CA certificates
  and tzdata — which `scratch` does not, and the update check needs — and a
  non-root uid. The alternative is `alpine`, whose only real advantage is a
  shell for `docker exec`; tommy is one static binary with an HTTP API and a UI,
  so the debugging value of a shell is low and the attack surface is not. Record
  the choice in `docs/docker.md` so it is not re-litigated.
- **Every default port is ≥ 1024**, so the image runs as non-root with no
  capability grants: 8811/tcp (UI and API), 8822/tcp (ingress), 1025/tcp
  (`smtp`), 2121/tcp (`ftp`), 2222/tcp (`sftp`), 2049/tcp (`nfs`), 2575/tcp
  (`hl7` MLLP), 6969/udp (`tftp`), 1162/udp (`snmp` trap). `EXPOSE` all of them,
  with the udp ones marked `/udp` — a `docker run -P` that silently omits the
  trap receiver is a bad first experience. **Take the list from 13·0's listing,
  not from this paragraph**; 13·6 asserts they agree.
- **The bind address is the trap in this task.** `config.DefaultBind` is
  `127.0.0.1`, which is right for a binary on a laptop and useless in a
  container: a published port never reaches a loopback listener, so the image
  must default to `--bind 0.0.0.0`. **Do not change `DefaultBind`** — a fake that
  listens on every interface by default is a worse default for the common case.
  While you are here, verify that `--bind` actually reaches the *listener*
  providers (SMTP, FTP, SFTP, TFTP, NFS, MLLP, trap) and not only the three core
  listeners; the container is the first place a provider that ignores it would
  show, and if one does, fixing it belongs in this task.
- **`ENV TOMMY_NO_UPDATE_CHECK=1` in the image.** A container should not phone
  GitHub on every start, and there is a sharper reason: the kleiner update check
  dereferences a nil version when GitHub is unreachable (wave 15, *Upstream:
  kleiner*), so an image started on an air-gapped network would panic at
  startup. Setting the variable in the image is the fix that does not wait on an
  upstream release.
- **A writable `/data` volume.** `as2` generates its identity on first use under
  the config directory, and a distroless non-root image has no writable home, so
  the default command must point it somewhere real (`--as2-cert-dir /data/as2`)
  and `/data` must be declared a `VOLUME`. This is also where `--persist` will
  land when wave 15 builds it — choose the path with that in mind rather than
  moving it later.
- **FTP passive mode is the most likely bug report.** `--ftp-passive-host`
  defaults to `127.0.0.1`, which a client *outside* the container will dial back
  to itself; a working `docker run` therefore needs a pinned passive range
  published as ports and a passive host the client can reach. Both flags already
  exist (`cmd/files.go`) — no code is needed, but `docs/docker.md` must carry a
  command that was actually run, and the compose file must set them.
- **Ship `docker-compose.yml` at the repository root**, alongside the run
  command. It is the honest way to express ten published ports plus a volume
  plus a config mount, and it is what people will paste into their own stack.
  `tommy.toml` already anticipates it — its `host` key exists precisely for
  "reached under a name other than localhost, inside docker-compose, for
  instance."

#### Configuration: full functionality by default, narrowed by a mounted file

This is the half that makes the image usable by a team rather than only by a
demo, and it is a design decision, not a paragraph of documentation.

- **Default is everything.** `tommy serve` with no config runs every compiled-in
  plugin and provider (`DefaultEnabled` is true, and a plugin or provider that
  says nothing inherits it). The image must not narrow that — someone who runs
  `docker run can3p/tommy` gets the whole surface, exactly as the binary does.
- **The image ships the repository's own `tommy.toml` at
  `/etc/tommy/tommy.toml`** and the default command is
  `serve --bind 0.0.0.0 --config /etc/tommy/tommy.toml`. Narrowing is then one
  bind-mount and no flags to remember:
  `-v ./tommy.toml:/etc/tommy/tommy.toml:ro`. The checked-in example is
  default-equivalent by construction ("every value below already matches
  tommy's built-in default"), so shipping it changes no behaviour.
- **Why the flags stay on the command line.** `cmd/serve.go` loads `--config`
  first and applies flags *over* it, and `--bind` additionally clears the
  per-listener binds — so a user's mounted config copied from the repository
  example, with `bind = "127.0.0.1"` still in it, cannot silently break the
  container. Verify this ordering still holds rather than trusting this
  sentence; it is the whole reason the mount is safe.
- **The alternative, rejected**: leave `--config` off the default command and
  tell people to add it when they mount. It loses `--bind 0.0.0.0` the moment
  anyone overrides the command, which puts the loopback trap back in front of
  every user who narrows their configuration. Say so in `docs/docker.md`.
- **`tommy providers --config /etc/tommy/tommy.toml` is the answer to "what did
  my config actually enable"**, and it runs in the image because the entrypoint
  is the binary. Document that, and document the single-plugin shortcuts
  (`docker run … tommy mail`) too — rule 10 says the CLI is level with the
  config, and the image inherits that for free only if someone writes it down.
- **Enforcement, not prose**: the checked-in `tommy.toml` claims to be
  default-equivalent and nothing checks it. Add the test — load the file, apply
  defaults, and compare against a bare `Config` with defaults applied — because
  once the image ships that file, the claim is load-bearing rather than a
  courtesy to readers.
- **The "keep it true" half**: a new CI job that builds the image for the host
  architecture (no push, no registry credentials, so it runs on pull requests
  from forks), starts it, and drives the README quickstart through the published
  ingress port — one `curl` to `/v3.1/send`, one `GET /api/v1/events?plugin=mail`
  asserting the event came back — then repeats it with a narrowed config mounted,
  asserting a disabled provider is really gone. Take the commands from
  `docs/docker.md` rather than duplicating them in the workflow, so the
  documented command is the one CI proves. Rule 12's third section applies to
  this file as it does to every provider README: every command in it has been
  run.

### 13·3 — publishing on every release

Owns: `.goreleaser.yaml` (`dockers_v2`), `.github/workflows/release.yml`.

- Use GoReleaser's **`dockers_v2`** section, not the deprecated `dockers` +
  `docker_manifests` pair. It arrived in GoReleaser v2.12, is in the free
  distribution, and builds and pushes the multi-architecture manifest in a
  single `docker buildx build --push` during the publish phase. The release
  workflow already pins `version: "~> v2"`, which resolves new enough.
  `dockers_v2` copies the prebuilt binary from `$TARGETPLATFORM/` in the build
  context, so the `Dockerfile` from 13·2 must be written for that layout
  (`ARG TARGETPLATFORM` / `COPY $TARGETPLATFORM/tommy /usr/bin/tommy`) — this is
  the one coupling between the two tasks, and it is why 13·2 lands first.
- **GoReleaser does not log in to registries itself.** The workflow needs
  `docker/setup-qemu-action`, `docker/setup-buildx-action` and
  `docker/login-action` before the GoReleaser step, in that order.
- **Tags to push**: `{{.Version}}` always; the moving `latest` only for a
  non-prerelease, so a `v0.2.0-rc1` never becomes what `docker pull can3p/tommy`
  gives you. Major and major.minor tags (`0`, `0.1`) are cheap and conventional
  — add them under the same non-prerelease condition.
- **Mirror to `ghcr.io/can3p/tommy` in the same run.** It costs one more entry
  in `images:` and no new secret — the workflow's own `GITHUB_TOKEN` with
  `packages: write` is enough — and it gives anyone who dislikes Docker Hub's
  rate limits a second source. Set
  `org.opencontainers.image.source=https://github.com/can3p/tommy` as a label,
  which is what links the package to the repository on GHCR.
- Labels/annotations to set: `source`, `description`, `licenses=MIT`, `version`,
  `revision`, `created`. They are what `docker inspect` and every registry UI
  read.
- **The one thing CI cannot prove.** The push path only executes on a real tag,
  so it is untestable on a pull request. Cut the first tag as a **prerelease**
  (`v0.1.0-rc1`) deliberately, confirm the manifest lists both architectures
  (`docker buildx imagetools inspect can3p/tommy:0.1.0-rc1`) and that `latest`
  did *not* move, and only then tag `v0.1.0`.

### 13·4 — the Docker Hub page

Owns: `docs/dockerhub.md`, `.github/workflows/release.yml` (one job).

Docker Hub shows two fields and renders neither the website nor a relative link:
a **short description** capped at 100 bytes and a **full description** capped at
25,000 bytes.

- **Why this is a new file and not the README.** Rule 14 says documentation
  surfaces render the sources rather than restate them, and this is the one
  surface that cannot: Docker Hub is a copy, in someone else's database, with a
  byte cap the README already exceeds. Resolve it by making `docs/dockerhub.md`
  a deliberately *thin* pointer page rather than a second README — what tommy
  is in two sentences, the one `docker run` line, the quickstart `curl` and its
  expected output, how to mount a config to narrow it, the port table, and then
  absolute links to the website for everything else. **No per-plugin or
  per-provider prose**: that is exactly the content that would drift, and it is
  one click away.
- Keep the short description in the same file (a front-matter key or a first
  heading the job extracts), so both fields have one source.
- **Keep it true**: a test asserting the full description stays under 25,000
  bytes and the short one under 100 — the action truncates silently otherwise —
  and 13·6's port test covers the table.
- Publish with `peter-evans/dockerhub-description` **on a tag push**, so the page
  is refreshed with every released version without anyone remembering to, plus
  `workflow_dispatch` so a typo can be fixed without cutting a release.

### 13·5 — the documentation and the website

Owns: `README.md`, `docs/catalogue.md`, `website/site.go`, `website/templates`.

The image is worthless if the only place it is mentioned is a workflow file.
Every surface a reader arrives at has to know tommy can be run without a Go
toolchain.

- **`README.md`**: `docker run` becomes the *first* thing under the 30-second
  quickstart, ahead of the source build, with the compose file as the second
  step for anything involving FTP or a narrowed config. Add an *Installation*
  section naming all three routes — release archive, Docker image, `go install`
  — and link `docs/docker.md`.
- **`docs/docker.md` is the single source** for how to run the image; every
  other surface links to it. It carries rule 12's three sections like any
  component README: what the image is, when you reach for it (CI, a docker
  compose stack, a machine with no Go), and commands that have been run.
- **The website publishes it.** Add `docs/docker.md` to the page list in
  `website/site.go` next to the other `docs/` entries, titled *Docker*, and give
  it a place in the landing page's navigation — a reader who lands on the site
  and cannot find how to run tommy has been failed by it.
- **`docker-compose.yml` and `Dockerfile` will surface as links to unpublished
  repository files.** `TestLinksToUnpublishedFilesAreKnown` will fail until they
  are added to its list, which is the gate working as designed: publish or
  deliberately link to GitHub, and record which. Do not delete the assertion.
- **The port table is generated, never typed.** After 13·0 the site can render
  it from `tommy providers --json`, which `website/` already shells out to.
  Hand-writing it into `docs/docker.md` would be a second copy of a fact the
  binary knows (rule 14).
- **`docs/catalogue.md`** gains nothing but a one-line pointer: it indexes
  plugins and providers, and the image is neither. Resist the urge to grow it.
- **`docs/clients.md`** should say what changes when tommy runs in a container:
  the base URL an SDK is pointed at is the *published* port, and FTP's passive
  host is the one setting a client cannot discover for itself.

### 13·6 — make it stay true

Owns: `CLAUDE.md`, `docs/implementation-plan.md` (§0), the three tests below.

Everything above is a snapshot. A wave that adds a listener provider, moves a
default port or renames a flag silently invalidates the `Dockerfile`, the compose
file, `docs/docker.md` and the Docker Hub page — and none of it fails, which is
precisely how the documentation went stale before. Encode the rule, then make a
test hold it.

- **A new `CLAUDE.md` rule.** *The image is a supported surface: a change to a
  listener port, a default, a flag or the config schema is not finished until
  the `Dockerfile`, `docker-compose.yml` and `docs/docker.md` reflect it.* Put
  it with the other plugin/provider rules, in the same voice, and say what it
  is protecting against — a user whose `docker run` line stopped working two
  releases ago.
- **A new step in *Finishing a wave*.** Between the current steps 7 (per-component
  documentation) and 8 (commit the documentation): *update the Docker surface* —
  the compose file, `docs/docker.md`, and the Docker Hub page if what it says
  changed. The Docker Hub page itself needs no manual step, because 13·4
  republishes it on every tag; what needs the step is the file it is generated
  from.
- **Test 1 — ports.** Every listener provider in the registry (13·0) has a
  matching `EXPOSE` in the `Dockerfile`, with the right protocol, and a
  published port in `docker-compose.yml`. A new provider fails the build until
  the image knows about it. This is the test that makes the rule real.
- **Test 2 — the shipped config.** The checked-in `tommy.toml` is
  default-equivalent (13·2), and it is the file the image installs at
  `/etc/tommy/tommy.toml`. Assert both: the equivalence, and that the Dockerfile
  copies *that* file rather than a divergent copy.
- **Test 3 — the Docker Hub page fits.** Under 25,000 bytes and under 100 for
  the short description, asserted in bytes and not in characters.
- **The commands in `docs/docker.md` are executed by CI** (13·2), which is the
  fourth check and the one that catches everything the other three cannot.
- Add these to the *keep it true* list in §4 alongside the OpenAPI descriptions,
  the site's coverage test and the per-component READMEs, so the next session
  reads them as one family rather than four accidents.

### The credentials the workflows read — provided

Nothing in 13·0 through 13·2 needs a secret. 13·3 and 13·4 read two, and both
are **already configured** on `can3p/tommy`; this table records what they are so
a future session can tell a missing secret from a broken workflow:

| Name | Kind | What it is |
|---|---|---|
| `DOCKERHUB_USERNAME` | repository **variable** is enough | The Docker Hub account that owns `can3p/tommy`. Not secret; a variable keeps it readable in logs where a masked value is only confusing. |
| `DOCKERHUB_TOKEN` | repository **secret** | A Docker Hub Personal Access Token (Account Settings → Personal access tokens), scope **read/write/delete**. Pushing images alone needs only read/write, but the description API used by 13·4 is documented as needing read/write/delete, so one token with the wider scope is simpler than two. |

- `GITHUB_TOKEN` is provided automatically; the GHCR mirror needs only
  `packages: write` added to the release workflow's `permissions` block. No
  second registry secret.
- **Confirm the `can3p/tommy` Docker Hub repository exists and is public**
  before the first tag. The description job in 13·4 needs the repository to
  exist, and push-to-create leaves its visibility at whatever the account
  default is — the first failure would otherwise be a released image nobody can
  pull.

### Definition of done

A tag push produces: release archives carrying `LICENSE`, a multi-architecture
image on Docker Hub and GHCR whose `latest` moved only if this was not a
prerelease, and a Docker Hub page whose `docker run` line has been executed by
CI. `docker run can3p/tommy` gives a stranger every plugin tommy has, and one
bind-mounted `tommy.toml` narrows it. The website tells them so. And a later
wave that moves a port fails its build until the image follows.

Then the wave-closing ritual in `CLAUDE.md` — which this wave has itself
extended, so run the version it leaves behind, not the one it started from.

---

## Wave 14 — tier 2 protocols

Bigger, still worth doing, roughly in this order. Each is a self-contained agent
task once its plugin core exists; none block each other.

**AS2 is built** and is in `docs/archive/history.md`. What it proved for the rest
of this list: a plugin whose providers need a *generated credential* now has a
worked pattern — `Deps.ConfigDir`, an identity handed to providers through a
binder, and generation deferred to first use. Reuse it rather than reinventing
it, and read the laziness rule in `docs/contracts.md` before writing anything
that creates a file.

| Candidate | Shape | Notes |
|---|---|---|
| **Modbus TCP** | own plugin, TCP | Small protocol, good Go libraries, and it lands on the **state-plus-event pattern** the `files` VFS proved: the register bank is state, writes are events, reads are polls. View is an editable register grid. Note it inverts the usual value — clients mostly *read*, so tommy supplies data rather than capturing it. |
| **SNMP agent** | extends `plugins/snmp` | Same state-plus-event pattern with an OID tree; pairs with the Wave 6c trap receiver but is a bigger lift. |
| **ISO 8583** | own plugin, TCP | `moov-io/iso8583` is excellent and bitmap decoding is painful enough that a decoded-field view has real value. Reply with a fixed approval and stop there, or it becomes scenario mocking. |
| **SMB2** | `files` provider | Perfect conceptual fit — another provider over the same VFS — and the highest cost here. Server-side SMB2 in Go is thin (negotiate, session setup with NTLMSSP, tree connect, create/read/write/close, find) and it drags NTLM along. Worth it eventually because the Docker-Samba pain is real. |

**Not planned** (and why): Kerberos KDC is the wrong *shape* — there is nothing to
inspect, the value is "authentication succeeded", which is a stub not a catcher.
RADIUS/TACACS+ are held back by accept-versus-reject being policy, though RADIUS
is the most likely to graduate. NTLM is not a plugin at all — it is a mechanism
inside SMB, implemented if and when SMB2 is. IBM MQ is a proprietary binary
protocol without a usable public specification. BACnet is large with thin Go
support; strictly after Modbus.

---

## Wave 15 — cross-cutting

Independent of each other and of the protocol work; each is one agent.

| Task | Owns | Notes |
|---|---|---|
| **TLS ingress** | `core/server/**`, config | `--tls` with a self-signed certificate generated on first run and written beside the config so it can be trusted once. Print the fingerprint. **Wave 8 already built the half you need**: `Deps.ConfigDir` is the directory of the config file (empty for a config built in memory), and `plugins/as2/identity.go` is a worked example of loading-or-generating a key pair with the paths configurable — which they must be, because tommy may run in a cluster that already has its own CA. Generate on **first use, not at startup**: doing it eagerly is what put a private key in the user's own config directory during `make check`. This is the documented route for non-Go SDKs that will not take a base URL (see `docs/clients.md`). **The seam already exists**: Wave 7 built `newHTTPServer` + `listenerOptions` in `core/server/httpserver.go`, and TLS is a field added there rather than a second construction path. Use `net/http`'s `Server.Protocols` for ALPN, not `golang.org/x/net/http2` — that module's `h2c` package is deprecated and would fail the staticcheck gate. |
| **Persistence** | `core/store/**`, `core/blob/**` | Opt-in `--persist <path>` snapshotting events and blobs. The `Store` and `BlobStore` interfaces were built for this; no plugin should need to change. Keep it dependency-free — files on disk, not SQLite — unless a real need appears. |
| **Search** | `core/server/api`, `core/server/ui` | Full-text across captured bodies. Currently `Query.Search` is a substring match; if that stops being enough, this is where it goes. |
| **Upstream: kleiner** | — | Fix `MaybeNotifyAboutNewVersion` in `can3p/kleiner`: it prints the error and falls through to dereference a nil version, panicking a released binary at startup when GitHub is unreachable. Second latent deref on the same path. Affects every project scaffolded from kleiner. |

## Backlog — small, unblocked, good first tasks

- **Link each plugin's OpenAPI description from its tab.** The shell links the
  events document from the status bar; a per-plugin link needs either the
  how-to-test panel to learn the API base or the shell to learn which plugins
  have a document (`snmp` has none). Left out of wave 10·1 rather than
  half-wired.

- **Listener providers hand out no event link.** Wave 9 answers every *ingress*
  response with `X-Tommy-Event-URL`, but SMTP, FTP, SFTP, TFTP, NFS, MLLP and
  the trap receiver have no response that can carry a header. The equivalent
  would be a log line naming the URL of each captured event — genuinely useful
  in local development, noise in CI, and it needs the UI origin plumbed into
  `Deps`, which nothing else wants. Deliberately not built rather than guessed
  at; if it is wanted, make it a flag rather than a default.
- **The event page renders the plugin fragment on every request.** It dispatches
  an in-process sub-request to `/ui/<plugin>/events/{id}`. That is cheap today
  because every such route reads one event from the store, and it is a
  requirement on plugin authors now written into `docs/contracts.md`: keep the
  fragment route side-effect free. If a plugin ever needs something expensive
  there, the page is where it will show.

- **NFS event granularity.** One logical upload over NFS is a CREATE plus one
  `files.upload` per WRITE chunk, because NFS has no open/close on the wire and
  `COMMIT` never reaches the backend. Deliberate — a debounce would make reads
  see stale content — but if the files tab ever looks noisy, this is why.
- **NFS records the mounting connection's peer.** `go-billy`'s methods take
  neither a context nor a connection, so per-client identity can only come from
  `Handler.Mount`'s `net.Conn`. For a kernel client that is the same host on a
  possibly different source port. Fixing it needs a core change nobody else
  wants.
- **A varbind table for the `snmp` tab.** The plugin deliberately shipped with no
  UI of its own, and the generic event view carries it: the list line names the
  version, trap OID and varbind count, and the detail pane renders each varbind's
  OID, type and value. A table would read better if anyone spends real time in
  that tab; nothing is blocked on it.
- **The `push` tab has had no polish pass**, like `files` and `chat` before it.
  The lock-screen archetype works and distinguishes a silent push honestly, but
  it has not been looked at with fresh eyes.
- `event.DecodePayload[T]` in core. `sms`, `chat` and `hl7` each carry a
  near-identical pointer/value/JSON-round-trip payload decoder, ~25 lines apiece.
  A convenience rather than a gap, reported by the third plugin to write one.

- `sms.New(sms.WithProviders(...))` uses an options pattern while `mail.New`,
  `files.New` and `chat.New` are variadic. Unify.
- Two shapes in the Mailjet fake have no primary source and are marked as such in
  the code: the 401 error code (`mj-0015`), and the response to exhausted blob
  capacity, which is not a Mailjet concept at all.
- SFTP defaults to `NoClientAuth`, so credentials are only presented — and
  therefore only recorded — when something is pinned. Deliberate, since it makes
  the cold-start snippet work with real OpenSSH, but revisit if capture matters
  more than convenience.
- The `files` and `chat` tabs have not had the polish pass mail and sms got.
- **`ProviderConfig.Port` is inert for HTTP providers — implement it or delete
  it.** `tommy.toml` advertised a dedicated listener per HTTP provider from the
  moment the config landed, but nothing ever built one: the ingress path-routes
  every HTTP provider onto one listener, and the field's only reader reports
  where a *listener* provider bound. `Validate` range- and collision-checks the
  value, which is what kept the gap hidden. Wave 6·0 removed the claim rather
  than implementing it. Either give a provider a listener of its own (worth
  having when a client cannot be pointed at a path, and it shares shape with the
  TLS ingress task in wave 15) or drop the field and its validation.
