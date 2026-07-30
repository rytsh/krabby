# Krabby Browser Importer

Chromium extension that renders pages with the user's browser session and returns cleaned HTML to a Krabby UI. It also fetches sitemap XML without page CORS restrictions.

## Install

1. If this came from the Krabby UI download, extract the ZIP first.
2. Open `chrome://extensions` (or `edge://extensions`).
3. Enable **Developer mode**.
4. Choose **Load unpacked** and select the extracted `krabby-browser-extension` directory.
5. Open the Krabby UI.
6. Open the extension popup and choose **Connect this origin**.
7. Reload the Krabby UI. The Add Content page will show that the extension is connected.

The extension accepts render requests only from origins explicitly connected through its popup. Rendered pages open in inactive tabs and are closed after their visible text stabilizes.
