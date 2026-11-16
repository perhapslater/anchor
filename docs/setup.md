# Docker setup wizard

Persist Anchor's `/config` directory in a private Docker volume. It contains the database, accounts and integration credentials. The repository's `compose.yaml` is a localhost-only HTTP example for testing; use Tailscale Serve or an HTTPS reverse proxy for deployment.

Start the container and open its logs, for example `docker compose logs anchor`. While the installation is unclaimed, Anchor prints a **one-time setup code** on each startup. Copy it into the setup page and create an administrator with a local recovery password. The code is invalidated after administrator creation and is never printed again. Existing log copies remain readable to anyone with access to those logs, including configured logging collectors. The `/config/setup-token` file remains an alternative before the code is consumed.

Passwords shorter than 12 characters are allowed. New-password fields show an amber advisory suggesting at least 12 characters; it does not block saving. Empty passwords are not accepted for local sign-in. A blank optional password field when editing an account keeps its existing password.

The wizard stages are **People → Applications → Notifications → Tailscale (optional) → Review setup**. The top progress navigation remains visible while scrolling. Its compact mobile header names the current stage and retains the optional Tailscale label. On the Tailscale page, **Skip for now** beside the page heading advances to Review setup and records the step for later. It preserves your current sign-in and any saved links. Return to Administration > Connection info > Tailscale to manage private access. Stage explanations live in the page body. Save or cancel drafts before continuing. Progress survives a browser reload or container restart, and Exit setup returns to Administration. Saved progress from earlier versions is migrated to the same named stage.

One shared action bar remains visible at the bottom. During normal wizard navigation it is labelled **Anchor setup**, and its primary button names the destination, such as **Next: Notifications**. Opening an application editor pauses wizard navigation and switches that same bar to **Application editor**. New applications use application-specific Back/Next buttons, **Cancel application**, and **Save application** on the final stage. Existing applications open directly on **Details**, with **Appearance**, **Connection** and **Advanced** tabs; **Cancel** and **Save changes** stay available from every tab. Saving validates the full application and reveals any invalid field in its tab. Preview remains optional. Changing an application type asks for confirmation if it removes existing connections and their credentials. Saving or cancelling returns to Applications and restores wizard actions without advancing the wizard. Footer space is reserved so it does not cover the last fields.

While setup is incomplete, **Setup wizard** in Administration reopens the saved stage. After **Finish Anchor setup**, this button is hidden and Administration opens on Applications. Accounts, integrations and notifications remain editable in their normal Administration pages, including options skipped during setup. Completed setup cannot be reopened by an older browser tab. During local development, use `dev.ps1 -Reset` to deliberately restart first-run setup.

Application/user removal, applying an import, switching away from edited identities/sign-in settings, and discarding unsaved changes use confirmation dialogs inside Anchor. Cancel or Escape keeps the data and draft unchanged; the safe action receives initial keyboard focus. Navigation through Anchor's links also asks before leaving unsaved changes. Browser controls such as reload, Back, or closing a tab do not display an unsaved-change warning; save or cancel drafts before using them.

To remove an account, open **Users & groups → Account details → Remove user** on that person's card. Anchor permanently removes their account, password, Tailscale link and local sessions. It removes their direct user grants from applications and announcements; if no audience remains, the item becomes administrators-only. Group-based access for other users and upstream service accounts are preserved. Removing your own account signs you out. At least one enabled administrator with a local recovery password must remain, so the last such account can be edited but cannot be removed, paused, demoted or have local sign-in disabled. The server enforces this protection even for direct API requests or simultaneous changes.


## Administration navigation

**Applications** contains all application tiles, including Seerr and Ebook library connections; there is no separate Integrations page. Each connected application shows **Configured** or **Needs setup**. These describe saved configuration, not a successful live connection test. Expand **Connection details** for provider addresses, credential presence, optional services and library IDs. Credentials stay masked and retain their existing storage/export rules. Existing tiles with both Seerr and Ebook features keep both connections and expose both in the Connection editor.

**Notifications** uses **ntfy** and **Webhook** tabs. Both destinations can be enabled at the same time. Switching clients retains their drafts, and saving one client preserves the other client's unsaved fields and credentials. Each client has its own save, test and delivery result; save that client's changes before testing it. Discord and Generic JSON remain Webhook formats. Leaving the section still asks before discarding drafts. Tabs support arrow keys, Home and End.

## Review and connection details

The final Review setup stage shows the configured deployment mode, browser address, browser transport and current sign-in method alongside the saved setup summary. **Connection info > Overview** in Administration is a read-only view of these details and the current account. Its card has no duplicate heading or account editor. The **Tailscale** tab in the same section contains setup guidance, account linking and sign-in/access validation; Tailscale remains a separate optional step in the first-run wizard. Connection information is not a separate wizard stage.

**Users & groups** is the single administrator editor for account details, passwords and **Allow local password sign-in**. Turning local sign-in off removes the saved password and ends password sessions; a linked Tailscale identity must remain. Turning it on requires a local login and a password if none is saved. **Keep a recovery route** on that page expands to show recovery instructions. The last enabled administrator with a local password cannot lose that recovery route.

Administrators use Administration rather than a My account navigation button. Members retain **My account → Local sign-in details** to edit their own local login and optional new password when local sign-in is enabled. Members cannot re-enable local sign-in disabled by an administrator or edit other accounts/permissions. A password-authenticated member must provide their current password; credential changes end password sessions and require signing in again. A trusted Tailscale identity authorizes the same account's local credential changes without its old local password, and the page confirms the save without redirecting, reloading or ending Tailscale access. Blank new passwords retain the saved password.

All password-authenticated users have the same **Sign out** control in the header. It ends only the current browser's local session and asks inside Anchor before discarding unsaved account/Administration edits. Tailscale-authenticated users have no header sign-out control or persistent authentication status label: Anchor cannot sign a device out of Tailscale, and Serve supplies its identity on each request. The sign-in method remains visible in Connection info for administrators and My account for members. The control follows the actual authentication method, rather than whether the device happens to be connected to Tailscale. The login and first-administrator setup page remain available at `/account.html`.

You can complete the wizard with your existing local login before making deployment changes. The Tailscale page separates the configured deployment mode, browser HTTPS connection, current sign-in method, and enabled people with saved email links. Email links alone do not verify automatic sign-in or remote access. If a session expires, the check displays an inline explanation and keeps the page open; authentication is still required to save settings.

**Test sign-in and access** checks the requesting device's real Tailscale identity, enabled Anchor account, groups and permitted applications using the same permission rules as the dashboard. A password cookie cannot make the test pass. **Test this connection** checks the address already open in this browser. To test the deployment from a localhost or LAN setup page, enter the full HTTPS address printed by Serve (including its port) and choose **Open Anchor through Tailscale**. A separate test tab reports its result back to the authenticated administrator's setup page without replacing the administrator's session. If the test has not returned after about 30 seconds, the page explains how to check the device's connection, address, network permissions and Serve configuration; this does not establish that Tailscale is disconnected. A missing identity similarly asks the user to connect Tailscale and use the Serve address. Recovery actions refer to Step 1 or Step 2 rather than relative directions.

A green **Automatic sign-in verified** result requires a real Tailscale-authenticated request over HTTPS and names the current person. Password sessions, disabled deployment mode, unlinked or paused accounts, HTTP and development simulations cannot produce a verified result. Test results show the groups and applications available to that account, including maintenance/unavailable states. Server health information does not test application reachability from the user's device or sign them into the applications. The result offers **Open this account's dashboard** to try applications. Each person must test their own sign-in. The standalone test's **Copy test link** shares a reusable link without its temporary result token.

**Preview someone's configured access** shows an enabled account's permitted applications without verifying that person's Tailscale sign-in. This preview is explicitly separate from an actual test. Results are temporary, never saved in the database, and invalidated when saved settings change; editing an identity or test address clears the current test. The test never starts Serve, changes modes, revokes sessions or changes accounts, groups or application permissions. It cannot certify connectivity from outside your home. Setup remains optional and can be completed without passing the test.

## Tailscale setup

The Tailscale page first explains two benefits: private HTTPS access from approved connected devices, and automatic sign-in to an existing Anchor account. For example, connect your phone to Tailscale, open Anchor's private address, and see the dashboard your Anchor permissions allow. Local password sign-in can also be used over the connection. Applications such as Plex retain their own sign-in and must have reachable URLs; linking an email does not configure applications, install Tailscale, invite people, or grant tailnet access.

Choose **Set up Tailscale** to reveal **Connect Anchor to Tailscale → Link people to their Anchor accounts → Test sign-in and access**. Existing links open the guide automatically on a new page load. In the connection instructions, choose where Tailscale runs and edit Anchor's listening port and Serve's HTTPS port. Container setups also take the name of the container running Tailscale. Generated commands can be selected or copied; invalid ports/container names do not produce executable commands. The generator is guidance only: changing its fields does not change Anchor's listener or deployment. Run deployment commands separately after reviewing existing mappings.

### Server

This path runs Tailscale on the computer hosting Anchor, separately from Anchor's Docker container. Tailscale does not need to be bundled in Anchor's image. Install and update it using the method supported by your server's operating system.

1. Install Tailscale on the server and connect it to your tailnet.
2. Configure Anchor and the server's Tailscale to share localhost, using the platform-specific examples below. Preserve Anchor's `/config` volume. Set `ANCHOR_MODE=tailscale` and `ANCHOR_LISTEN=127.0.0.1:8080` (or your chosen free Anchor port), then restart it. Remove an old HTTP `ANCHOR_PUBLIC_URL`, or set it to the correct HTTPS Serve origin. An ordinary bridge container published at a LAN address does not meet Anchor's current loopback identity trust requirement.
3. In your **server terminal**, first check the existing configuration:

   ```sh
   tailscale serve status
   ```

4. Choose an unused Serve HTTPS port. The following example forwards HTTPS port 8443 to Anchor's listening port 8080:

   ```sh
   tailscale serve --bg --https=8443 http://127.0.0.1:8080
   ```

   Review existing mappings before starting: a command on an already configured HTTPS port can replace its mapping. Do not use Funnel. Follow Tailscale's HTTPS activation prompt if one appears.

5. From a Tailscale-connected device, open the **HTTPS address printed by Serve**. Use it for first-run setup, reading the one-time code from Anchor's container logs. With Anchor bound to localhost in this setup, its HTTP port is not available at the server's LAN address.
6. Link the administrator's Tailscale email in the wizard and use **Open Anchor through Tailscale** to test the HTTPS Serve address, or **Test this connection** if that address is already open.

<details>
<summary>Platform-specific setup examples</summary>

- **Linux with Docker:** use host networking for Anchor: `--network host`, or `network_mode: host` in Compose. This shares the server's localhost. Run Serve with the privileges required by your Tailscale installation.
- **Unraid example:** install the Tailscale plugin and set Anchor's Network Type to Host. Unraid's **per-container Use Tailscale / Serve integration** is a separate path: select **Container** when using it.
- **Windows / macOS with Docker:** container localhost can differ from the server's localhost. Verify that your Docker configuration shares the required loopback connection. A Tailscale container sharing Anchor's network environment provides the alternative container-based path.

</details>

These examples describe different ways to meet the same loopback identity requirement. Merely running Anchor and Tailscale on the same physical computer does not establish it.

### Container

Run the official Tailscale container beside Anchor and share their network environment. With Docker Compose, Anchor uses `network_mode: service:tailscale`, replacing `tailscale` with the Tailscale service name. Both then share localhost; merely putting them on the same Docker bridge network is insufficient. Persist Tailscale's state and Anchor's `/config` separately. Configure Anchor's mode/listener as above and avoid a separately published Anchor port. Update Tailscale by updating its container image while preserving its state volume.

Run the generated commands on the **Docker host**, using the name of the container that actually runs Tailscale. Example:

```sh
docker exec tailscale-anchor tailscale serve status
docker exec tailscale-anchor tailscale serve --bg --https=8443 http://127.0.0.1:8080
```

### Local Development

Choose **Local Development** to generate `dev.ps1` arguments matching your ports. This option uses the project's Windows PowerShell launcher. Run from PowerShell in the repository:

```powershell
.\dev.ps1 -Port 18080 -Tailscale -TailscalePort 8443
```

The launcher creates a temporary Serve session and removes its session when stopped. It preserves development data; add `-Reset` only when intentionally restarting setup from scratch.

### Identity and verification

**Tailscale sign-in email** means the email address the person uses to sign into Tailscale, for example `alex@example.com`. Each person appears with their saved email and account/link status. Choose **Link email** or **Edit link** to open the editor dialog. **Use current Tailscale email** fills the identity observed on this device; confirm it belongs to the selected person and save to link it. No identity is linked automatically. Duplicate or invalid links keep the dialog and draft open with an inline error; canceling unsaved edits requires confirmation. Removing a link requires that person to retain a local password. Saving your own email through this identity editor preserves this administrator's current local session; other local sessions for the changed account end. In Users & groups, unchanged saves and name edits preserve local sessions. Changes to passwords, sign-in details, group membership or account status revoke the edited person's local sessions. Reordering groups, changing their case or repeating an existing group does not change effective membership. Keep a local administrator password for recovery.

Users still need an explicitly linked, enabled Anchor account for automatic sign-in. There is no automatic account creation in this flow. Anchor's accounts/groups determine permissions. Identity comes from the requesting device's Tailscale account, so switching browser profiles does not switch it. Connection info explains the administrator's current sign-in; members see their method and device identity in My account. When the Tailscale page is opened at an HTTPS `.ts.net` address, the test address is prefilled with that origin; otherwise paste the address from `tailscale serve status`. Anchor does not invent a server address or report network-wide success from one browser's check. Development identity simulation is labeled separately and cannot produce a verified connection result.

### Profile photos

Anchor uses the same profile image on the dashboard, user cards and My account: an uploaded photo takes priority, followed by the last trusted Tailscale photo received for that account, then initials. Provider photos are remembered when that linked person accesses Anchor through Serve; they are not looked up for people who have never signed in. The saved image also applies during password sign-in. Changing a Tailscale email clears its old provider image, while an uploaded override remains. Missing or failed images use initials.

Administrators use **Users & groups → Edit → Profile photo**. Save a new user first before adding their photo. Members use **My account → Profile photo**, including when local password sign-in is disabled. Choose a file and **Upload photo**, or **Use default photo** to remove an override. Photo changes save separately from account details, preserve other form drafts and never end sessions. Users can change only their own images; administrators can change any user's image.

Uploads accept PNG/JPEG up to 2 MB, at most 4096 pixels per side and 8 million pixels total. Anchor checks and decodes the image, crops it to a square and stores a new 256px PNG without the original metadata. Other formats and malformed images are rejected. Protected image routes serve stored photos only to their owner or an administrator. Full database backups include photos; YAML dashboard exports do not. Schema 6 adds photo storage without changing existing account credentials.

The dashboard avatar sits at the far right after navigation on desktop, and stays in the top-right corner as navigation wraps below on mobile. The greeting provides the name; the avatar retains an accessible description and name tooltip.

### HTTPS reverse proxy

For a separate HTTPS reverse proxy, configure `ANCHOR_MODE=proxy`, `ANCHOR_PUBLIC_URL=https://your-host` and a Anchor listener reachable only by that proxy. Proxy mode uses local accounts and does not trust forwarded identity headers. HTTP LAN mode is explicitly unencrypted and requires `ANCHOR_ALLOW_HTTP=1`.

## Applications

Applications has **Manage applications** and **Arrange dashboard** views. Manage adds/edits service cards. Arrange shows compact tiles grouped by dashboard section: drag a handle with mouse or touch to move an app within or between sections, or click it to choose a section and exact position. Save order applies the draft; Discard changes restores the saved order. Escape cancels an active drag. Leaving with an unsaved arrangement prompts before discarding it. Hidden applications remain labeled in the editor, and access rules remain attached to each application. Section order is controlled by Dashboard settings.

Enabled administrators automatically have access to every enabled application; you do not need to add the `admin` group to each application's audience. Choose **Administrators only** to exclude members, or select the other people/groups who should also have access. Disabled applications stay hidden and inaccessible to everyone until re-enabled in Administration. **Preview as** uses the selected person's permissions, and the Tailscale sign-in test reports the signed-in person's effective access. The same application permissions apply to shortcuts, Request status and ebook browsing/downloads. Announcement audiences remain separate.

The wizard uses the normal guided application builder: **Type → App details → Appearance → Connection → Review application**. Its progress explicitly says **Application step**, and its visible bottom actions name the next application stage. Standard shortcuts require no API credentials. Seerr/Plex connections, optional Sonarr/Radarr data and Audiobookshelf libraries use the credentials requested by their selected features. Credentials and configuration save together. The live preview starts collapsed for every new editing session; choose **Show preview** after Type to reveal it, or **Hide preview** to give the form more room. The builder's previews use labeled sample data; they are not live integration tests. URLs must be reachable from Anchor's container; loopback and link-local integration destinations are rejected.

For the full Request status experience, edit the **Seerr connection** and fill in **Episode progress & Coming up**: Sonarr's server URL/API key for show progress and episode dates, and Radarr's server URL/API key for movie progress and release dates. These must be connected within that Seerr source; adding separate Sonarr/Radarr shortcut tiles does not link them. Without either connection, Coming up is hidden and statuses use only Seerr/Plex data, which can differ from a fully connected instance. Request status uses the same wording, rows, posters, search and six-month calendar logic as the original Anchor implementation. Its setup preview shares the row renderer and uses sample titles with TV/movie artwork placeholders; Refresh now in a preview never calls your servers.

Anchor sign-in does not sign people into upstream applications. Hiding a tile does not replace an upstream application's own authorization.

## Notifications

ntfy and a webhook can be enabled at the same time. Each has independent outage/recovery and user-report selections, a **Send test notification** button, and its last delivery status since startup. Save before testing. ntfy requires a server, topic and a publishing token unless the server allows unauthenticated publishing. Subscribe to that topic separately in your ntfy app.

For a webhook, select:

- **Discord:** paste the HTTPS webhook URL from the channel's Integrations settings. Anchor sends a Discord message, disables automatic mentions and asks Discord to confirm message creation. It respects rate-limit responses.
- **Generic JSON:** supply an HTTPS receiver accepting POST requests with `Content-Type: application/json`. This is Anchor's event format, not an automatic adapter for every notification provider. Query-string credentials are supported; arbitrary authentication headers and custom payload templates are not included.

Example generic payload:

```json
{
  "schema_version": 1,
  "event": "alert",
  "title": "Plex is down",
  "message": "Anchor can't reach Plex.",
  "priority": 4,
  "tags": ["rotating_light"],
  "url": "https://plex.example.net"
}
```

`event` is `alert`, `report` or `test`. `url` may be empty and `tags` may be null. Messages can disclose application names/addresses and the name/identity of a person reporting a problem; choose private topics/channels as appropriate.

Webhook URLs are credentials: the UI reports only whether one is saved, and ordinary YAML exports exclude them. Full database backups include them. Network/upstream errors do not print the webhook URL or response body. Requests do not follow redirects, use environment proxies, or connect to loopback, link-local, unspecified or multicast destinations. Private LAN and tailnet receivers with valid HTTPS are supported.

Delivery to one destination does not prevent attempts to the other. Automatic alerts retry failed destinations while retaining successful receipts for that outage in memory. Receipts and delivery status reset on restart; delivery is not an exactly-once durable queue. A timeout can mean the remote service accepted a message without Anchor receiving confirmation. A user report delivered to only one destination reports partial success; its other destination is shown as failed in Administration. Test buttons send a single real test and report its result.

## Recovery and backups

Before finishing, review the setup summary and download a full database backup from Import & export if desired. Keep it private. For a forgotten administrator password, stop Anchor and run `anchor recover-admin YOUR_LOCAL_LOGIN` against the same `/config` volume, then restart and change the temporary password. See the README for restore instructions and deployment-validation limitations.
