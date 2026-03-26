# Google OAuth Setup

This guide walks you through creating Google OAuth credentials for Clawvisor.
Once configured, you can activate Gmail, Calendar, Drive, and Contacts from the
Clawvisor dashboard — all four share a single OAuth connection.

## 1. Create a Google Cloud project

1. Go to the [Google Cloud Console](https://console.cloud.google.com/)
2. Click the project dropdown at the top and select **New Project**
3. Name it something like `Clawvisor` and click **Create**
4. Make sure the new project is selected in the dropdown

## 2. Enable the APIs

Go to **APIs & Services → Library** and enable each of these:

- [Gmail API](https://console.cloud.google.com/apis/library/gmail.googleapis.com)
- [Google Calendar API](https://console.cloud.google.com/apis/library/calendar-json.googleapis.com)
- [Google Drive API](https://console.cloud.google.com/apis/library/drive.googleapis.com)
- [Contacts API](https://console.cloud.google.com/apis/library/contactsapi.googleapis.com)

You only need to enable the ones you plan to use, but there's no harm in
enabling all four.

## 3. Set up the Google Auth Platform

The Google Cloud Console uses the **Google Auth Platform** to manage OAuth.
You'll configure three sections: **Branding**, **Audience**, and **Data Access**.

### Branding

1. Go to [**Google Auth Platform → Branding**](https://console.cloud.google.com/auth/branding)
2. If you see "Google Auth platform not configured yet", click **Get Started**
3. Fill in:
   - **App name:** `Clawvisor`
   - **User support email:** your email
4. Click **Next**
5. Choose your audience type (see below), click **Next**
6. Enter your **developer contact email**, click **Next**
7. Agree to the Google API Services User Data Policy
8. Click **Continue**, then **Create**

### Audience

1. Go to [**Google Auth Platform → Audience**](https://console.cloud.google.com/auth/audience)
2. Select **External** (unless you have a Google Workspace org and want internal-only)
3. Under **Test users**, click **Add users**
4. Add your own Google email address and any other users who will test Clawvisor
5. Click **Save**

> **Note:** While in "Testing" status, only the test users you add here can
> authorize. You can add up to 100 test users before Google requires app
> verification.

## 4. Create OAuth credentials

1. Go to [**Google Auth Platform → Clients**](https://console.cloud.google.com/auth/clients)
2. Click **Create Client**
3. Application type: **Web application**
4. Name: `Clawvisor`
5. Under **Authorized redirect URIs**, add:
   - `http://localhost:25297/api/oauth/callback` (for local development)
   - If you're running Clawvisor on a different host/port, adjust accordingly
     (e.g. `https://clawvisor.yourdomain.com/api/oauth/callback`)
6. Click **Create**
7. Copy the **Client ID** and **Client Secret**

> **Important:** The client secret is only shown once at creation time. Store
> it securely — you won't be able to view it again in the console.

## 5. Configure Clawvisor

If you ran `make setup` and said **Yes** to Google services, the setup wizard
already prompted you for these values — you can skip this step.

Otherwise, set the credentials as environment variables before starting the server:

```bash
export GOOGLE_CLIENT_ID="your-client-id.apps.googleusercontent.com"
export GOOGLE_CLIENT_SECRET="your-client-secret"
```

Or add them to your `config.yaml`:

```yaml
services:
  google:
    client_id: "your-client-id.apps.googleusercontent.com"
    client_secret: "your-client-secret"
```

## 6. Connect your account

1. Start Clawvisor: `make run`
2. Open the dashboard and go to **Services**
3. Click **Connect** next to Google
4. You'll be redirected to Google's consent screen — authorize with your account
5. You may see a "This app isn't verified" warning — click **Advanced** then
   **Go to Clawvisor (unsafe)** to proceed (this is expected for self-hosted apps)
6. Once connected, Gmail, Calendar, Drive, and Contacts will show as active

## Troubleshooting

**"Access blocked: This app's request is invalid" (redirect_uri_mismatch)**

The redirect URI in your OAuth credentials doesn't match what Clawvisor is
using. Check that:
- The URI in Google Cloud matches your Clawvisor URL exactly
- You're using `http` (not `https`) for localhost
- The path is `/api/oauth/callback`
- Changes to redirect URIs can take 5 minutes to a few hours to take effect

**"This app isn't verified"**

This is normal for self-hosted apps in "Testing" status. Click **Advanced**,
then **Go to Clawvisor (unsafe)**. This only appears for test users you've
added in the Audience section.

**"Error 403: access_not_configured"**

You haven't enabled the API you're trying to use. Go back to step 2 and enable
the relevant API in the Google Cloud Console.

**"You can't sign in because this app sent an invalid request"**

Make sure you've completed the Google Auth Platform setup (Branding + Audience)
before creating your OAuth client. The client won't work without a configured
consent screen.

**Only some services work**

Each API must be enabled individually in step 2. If Gmail works but Calendar
doesn't, check that the Calendar API is enabled in your project.
