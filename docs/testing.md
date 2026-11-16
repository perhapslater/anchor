# Local feature review

Run .\dev.ps1 -Check. Use disposable accounts and dummy credentials during development. Your development database is .local/config/anchor.db and survives rebuilds.

To switch test identities, finish setup first, stop the development server and run `.\dev.ps1 -SimulateTailscale`. Open the printed localhost identity picker, choose an existing Anchor person and use the **Change test user** banner link to return. Enabled people with a Tailscale sign-in email authenticate through Anchor's normal Serve-header path, without requiring that email to belong to a real Tailscale account. Disabled/unlinked people and the explicit unlinked/signed-out choices exercise sign-in rejection. This mode ignores previous local-login cookies and does not create accounts or change their identity links. Password testing uses normal `dev.ps1`. Different browser profiles can select different simulated people simultaneously; tabs sharing a browser session share its selection. Changes made in the application still save to the existing local development database.

The simulator uses a separate `anchordev` build and binds only `127.0.0.1`. It validates the exact Host, request origin, selection token and signed session cookie. Normal builds exclude its implementation and assets and reject the simulation environment flag. `-Check` runs both the normal and development-tagged Go checks. `-SimulateTailscale` is mutually exclusive with `-Tailscale` and `-Reset`; it does not start or modify Tailscale Serve. Use real `-Tailscale` for HTTPS, device identity and profile-photo validation.

To restart first-run testing, stop Anchor with Ctrl+C and run `.\dev.ps1 -Reset` (optionally with `-Tailscale`). The launcher archives `.local/config` under `.local/config-backups` before starting fresh. This resets users, credentials, application settings and wizard progress; it does not undo source edits. The archives contain private credentials and are ignored by Git. Launcher reset checks use isolated temporary fixtures and run with `-Check`, or separately with `.\tests\dev-reset.tests.ps1`; they never reset the real development configuration.

1. On a fresh database, use the setup-token file to create your administrator. Reload: setup must not be offered again.
2. Add a custom application through Type, Basics, Appearance, Setup and Review. Check the live tile, audience selector, automatic icon/shortcut ID and Advanced settings. Try a welcome guide and phone-app instructions in preview, save, then edit them again. Turning either helper off must remove it without an invisible field blocking Save. Open the tile from the dashboard.
3. Edit its description and name. The stable ID and shortcut address must remain unchanged. Reorder, disable and re-enable tiles.
4. Create a member with a local password and a group. Restrict one tile to admin and one to the member's group. Preview the member, then sign in as the member in a separate browser profile. Admin screens and APIs must be denied.
5. Link a user's Tailscale identity in an isolated HTTPS Serve deployment. Check both sign-in methods resolve to the same permissions. Forged headers from direct non-loopback connections must not authenticate.
6. Change a user's password or disable the user. Existing local sessions must stop working. Attempt to disable the last administrator with a local password: it must fail.
7. Save settings from two administrator tabs. The second stale save must be rejected rather than silently overwrite the first.
8. Configure optional integration settings with dummy keys. Verify key fields show presence only and blank fields preserve existing values. Clear a key explicitly.
9. Export YAML: it must omit account records, passwords, sessions and API keys. Preview an import: nothing changes until Apply. Unknown schema versions, invalid URLs and multiple YAML documents must fail.
10. Download a complete database backup and restore it to a separate stopped installation. Check accounts, tiles and integration-key presence. Keep backups private.
11. Stop the server, edit a visible frontend string, restart with .\dev.ps1 and reload. Verify the rebuilt frontend appears without committing. Restore your temporary string change afterwards.
12. Check phone-width and desktop layouts, keyboard navigation, account changes with open dialogs, and browser refresh after an update.
13. Dismiss the HTTP connection notice with its X. Dashboard, My account, Administration and a browser reload must keep it dismissed on the same origin. Security must still show the deployment mode and connection status.
14. In Tailscale, select a member and save their Tailscale identity. In Security, select that member, disable local sign-in, and check that their password and local sessions stop working. Re-enable with a new password. Removing the last administrator's local password must fail. Use an isolated Tailscale Serve deployment to validate the linked identity on a real encrypted connection.
15. Create two Seerr connections and two ebook libraries through the builder. Use the same ebook ID in both libraries and verify catalog rows, covers and download addresses remain scoped to the selected tile. Verify a user allowed only one tile cannot read or download from the other. Reopen each connection from Integrations and check that saved credentials are not exposed. In Notifications, verify the provider is explicitly ntfy and save its token independently. Blank fields must preserve existing keys; a rejected save must change neither settings nor credentials.

Automated Windows validation includes Go vet/tests, process startup, SQLite persistence and backup restoration, setup/session/CSRF/permission boundaries, Tailscale header linking, optimistic saves, YAML isolation, and frontend/service-worker tests.

Before a public release: finalize project branding and publication metadata; complete the content and dependency-license audit; validate Docker and the actual Tailscale Serve deployment; run the Go race suite on Linux; run source and image vulnerability scans; test representative real integration versions and physical-device/PWA behavior. Project code already uses MIT. Do not infer those checks from a passing Windows suite.

## Isolated automated QA

Use Go 1.26.11 or a newer supported, security-patched release. The regular Go tests create temporary databases and synthetic upstreams; the opt-in browser test also starts its own disposable installation. Do not point either test at existing application data. Avoid invoking the local launcher for an isolated audit: it intentionally retains development state.

```powershell
go vet ./...
go test ./... -count=1
node --test --test-timeout=5000 tests/*.test.cjs
```

`tests/browser-qa.cjs` is a separate opt-in Playwright suite and is excluded from the ordinary frontend glob. Install Playwright outside this repository and build the application and Go test fixture into a new temporary directory:

```powershell
$qaDir = Join-Path ([System.IO.Path]::GetTempPath()) ('anchor-qa-' + [guid]::NewGuid().ToString('N'))
New-Item -ItemType Directory -Path $qaDir | Out-Null
go build -o (Join-Path $qaDir 'anchor.exe') .
go test -c -o (Join-Path $qaDir 'fixture.exe') .
npm install --prefix (Join-Path $qaDir 'tools') --no-audit --no-fund --ignore-scripts playwright@1.56.1
$env:ANCHOR_QA_BINARY = Join-Path $qaDir 'anchor.exe'
$env:ANCHOR_QA_FIXTURE = Join-Path $qaDir 'fixture.exe'
$env:ANCHOR_PLAYWRIGHT_MODULE = Join-Path $qaDir 'tools/node_modules/playwright'
node --test tests/browser-qa.cjs
Remove-Item Env:ANCHOR_QA_BINARY, Env:ANCHOR_QA_FIXTURE, Env:ANCHOR_PLAYWRIGHT_MODULE
```

The suite defaults to the standard Windows Chrome installation. Set `ANCHOR_QA_CHROME` to an existing compatible Chromium executable when needed. Optional `ANCHOR_QA_ARTIFACT_DIR` saves screenshots containing only synthetic accounts and data; choose a directory outside the repository. No browser download or project dependency is added. Both servers bind loopback; the large-catalog fixture announces its own ephemeral listener rather than attaching to the fixed manual-fixture port. The runner blocks page requests to other origins and disables Chromium background networking. Child processes, browser contexts and their temporary application data are cleaned up after the run.

To include accessibility checks for the app builder, preview dialog, integrations, notifications, Security and Tailscale, install `axe-core` alongside the external Playwright tools and set `ANCHOR_QA_AXE_PATH` to its `axe.min.js`. The checks cover WCAG A/AA tags and axe best practices. The builder is exercised in light and dark themes; the new administration screens also receive 320px reflow checks. These checks do not replace physical-device or assistive-technology review.

Keep test results, screenshots and assessment reports outside the repository. The validation and assessment directories are ignored and are not part of the public project documentation.

## Isolated UI and visual regression QA

`tests/ui-visual.cjs` is opt-in and runs independently of the frontend unit-test glob. It creates new OS-temporary SQLite installations, seeds synthetic administrator/member accounts using `TestUIVisualSeed`, signs in through the real application, and mocks integration responses in the browser. It does not use `.local`, the development launcher, a running household installation, or the host clipboard. The seed refuses an existing database. Servers, browser contexts and owned temporary databases are cleaned up when the runner exits normally or reports a test failure.

Use Go 1.26.11 or newer. Install the audit-only tools outside the repository; none is a production dependency. This example uses an existing Windows Chrome installation. Choose a new output directory outside the repository for every approved baseline and verification run.

```powershell
$uiDir = Join-Path ([System.IO.Path]::GetTempPath()) ('anchor-ui-tools-' + [guid]::NewGuid().ToString('N'))
New-Item -ItemType Directory -Path $uiDir | Out-Null
go build -o (Join-Path $uiDir 'anchor.exe') .
go test -c -o (Join-Path $uiDir 'fixture.exe') .
npm install --prefix $uiDir --no-audit --no-fund --ignore-scripts playwright@1.56.1 axe-core@4.11.0 pngjs@7.0.0 pixelmatch@7.1.0
$env:ANCHOR_UI_TOOLS = Join-Path $uiDir 'node_modules'
$env:ANCHOR_UI_BINARY = Join-Path $uiDir 'anchor.exe'
$env:ANCHOR_UI_SEED_BINARY = Join-Path $uiDir 'fixture.exe'
$env:ANCHOR_UI_ENGINE = 'chromium'
$env:ANCHOR_UI_PHASE = 'candidate'
$env:ANCHOR_UI_OUTPUT = Join-Path $uiDir 'candidate'
node tests/ui-visual.cjs

# Repeat the same source, OS, browser version and font environment.
$env:ANCHOR_UI_BASELINE = Join-Path $uiDir 'candidate'
$env:ANCHOR_UI_PHASE = 'verify'
$env:ANCHOR_UI_OUTPUT = Join-Path $uiDir 'verify'
node tests/ui-visual.cjs
```

Each output contains `gallery.html`, full-size PNGs and `manifest.json` with asset hashes, browser conditions, geometry checks, accessibility findings and console diagnostics. The full Chromium matrix covers seven widths in both themes; `ANCHOR_UI_QUICK=1` selects 320/1440px plus the supplementary states. For Firefox or WebKit, install their Playwright browsers into an external `PLAYWRIGHT_BROWSERS_PATH` and select `ANCHOR_UI_ENGINE=firefox` or `webkit`. `ANCHOR_QA_CHROME` overrides the Chromium executable. Rebuild the binary after frontend changes because the assets are embedded.

`reference` records original failures without declaring that design correct; `candidate` and `verify` fail on detected defects, unexpected JavaScript exceptions or unexpected error-console messages. `verify` compares screenshots using pixelmatch (0.1 per-pixel threshold, antialias exclusion, maximum 50 changed pixels per image, no masks). Use matching engine/version/OS; do not relax the threshold to clear a real difference. Freeze font availability and browser versions in CI as well. CSS viewport reflow probes are included at 720×450 and 360×225; they do not replace actual browser zoom or physical-device testing.

No pre-approved visual baseline exists. Inspect the candidate gallery at full size, review its geometry/axe results and source hashes, and compare significant fixes with the original references. Only after human review should an unchanged copy of that directory become the approved baseline: set its manifest's `approved` field to `true` and preserve it in the team's chosen artifact storage. The runner refuses to overwrite an approved output directory. Compare future runs against it using a separate output directory. Human approval is never inferred from a passing test.

Unset the `ANCHOR_UI_*`, `ANCHOR_QA_CHROME` and `PLAYWRIGHT_BROWSERS_PATH` variables when finished if they were set only for this run. Preserve results and approval candidates in private storage outside the repository.

## Tailscale launcher review

Run `.\dev.ps1 -SimulateTailscale -BuildOnly` to build the simulator without starting the application. In the opt-in browser suite, set `ANCHOR_SIMULATOR_BINARY` to this binary (or one built with `go build -tags anchordev`). The additional workflow uses a consistent backup of the suite's disposable database, starts a separate loopback simulator, switches administrator/member identities, verifies rejection and independent browser selections, and audits the identity picker in light/dark at 320px and 1440px. It verifies the production binary has no picker endpoint. No existing developer database or real Tailscale installation is used by this automated workflow.

Run `.\dev.ps1 -Tailscale` with an existing local test database and Tailscale connected. Open the printed HTTPS address and confirm My account says Tailscale and resolves to the linked user. An incognito window on the same PC uses the same device identity; use another user's Tailscale device for a second identity. Stop the launcher and verify `tailscale serve status` no longer lists its foreground session. A pre-existing mapping on the requested HTTPS port must cause startup to fail without changing that mapping. `anchor link-tailscale LOCAL_LOGIN user@example.com` provides a console maintenance path for existing accounts and should preserve their roles and passwords while revoking their local sessions on changes.
