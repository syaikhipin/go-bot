# Set up your Google Workspace application

Each user supplies their own Google OAuth application, just as X users supply
an X application. Go Bot does not distribute a shared Google client or require
application credentials in deployment `.env` files.

## Create the application

1. Create or select a project in the [Google Cloud Console](https://console.cloud.google.com/).
2. In **APIs & Services → Library**, enable APIs **as needed**. Drive operations
   use Google Drive API; email uses Gmail API; calendars use Google Calendar API;
   Docs, Sheets and Slides use their corresponding APIs. You do not need to
   enable every API before connecting. A disabled API returns an error when used.
3. In **Google Auth Platform → Branding**, configure your application name,
   support email and developer contact. Follow the console's requirements for
   the application information you provide.
4. Configure an **External** audience to use personal Google accounts or accounts
   outside an organization. While the application is in **Testing**, add the
   **Google account email** you will authorize under **Audience → Test users**.
   This whitelist is stored by Google, not in Go Bot or Redis. It is unrelated
   to your local Go Bot username. Workspace organization policies may also apply.
5. Open **Clients → Create client** and choose **Desktop app**. Download and keep its JSON immediately after creation; existing clients may no
   longer offer the original secret for download.
   Do not choose Web application, create a service-account key or put this JSON
   in the plugin source directory.
6. In Go Bot's Google Workspace plugin, select **Accounts and authorization →
   Connect Google account**. On Web, select the downloaded JSON file; it is parsed and submitted automatically.
   On mobile, paste the JSON in the protected input on first connection. Then select permissions, open the Google link and consent.
7. Copy the browser's complete localhost redirect URL into the panel even if
   its localhost page cannot open. Wait for the plugin to confirm connection.

The plugin validates the downloaded `installed` configuration and standard Google
endpoints, and saves it as native `~/.config/gws/client_secret.json` with private
permissions. That directory points into your Host data. Later connections reuse
it; account disconnect uses native gws logout and retains the application config.
User access and refresh tokens remain in the same native gws directory. Neither
Redis nor Go credentials receives a second copy.

## Testing and public use

Testing is suitable for your own application and nominated test accounts. An
unverified testing app can display a Google warning, and its grants can be
subject to testing limits. Only continue with an application you recognize.
A maintainer publishing an application for other people must separately satisfy
Google's publishing and applicable sensitive/restricted-scope verification
requirements. Downloading a client JSON does not grant public availability.

Gmail, Drive, Calendar, Docs, Sheets and Slides support personal Google accounts;
these features do not require a paid Workspace enterprise subscription. API
access still depends on enabled APIs, scopes, quotas and organization policies.

## Upstream setup option

On a trusted computer with an authenticated `gcloud` CLI, upstream
`gws auth setup` can guide project selection/creation and API enablement. Its
current Desktop-client stage still directs you to the Cloud Console. You can
also follow the manual steps above without installing gcloud on your phone or
inside the Host. Existing native gws authorization can be imported as described
in the [connection guide](plugin-account-setup.md).

Retrieved service data can enter the Agent context and the selected model
provider. Review requested permissions before consenting. Never put application
JSON, account exports, callback URLs or tokens in chat, source files or Git.

[Google Desktop credentials](https://developers.google.com/workspace/guides/create-credentials#desktop-app)
· [Consent screen and audience](https://developers.google.com/workspace/guides/configure-oauth-consent)
· [Google publishing requirements](https://developers.google.com/identity/protocols/oauth2/production-readiness/policy-compliance)
· [gws authentication](https://github.com/googleworkspace/cli#authentication)
