# joe-links Browser Extension

Navigate to `go/slug` links without typing `http://`.

<!-- Governing: SPEC-0008 REQ "Firefox Compatibility", ADR-0012 -->

## Browser Support

The extension uses Manifest V3 and works across modern Chromium and Gecko browsers. The `chrome.*` API namespace is used throughout; Firefox MV3 provides a built-in compatibility layer that maps `chrome.*` calls to the WebExtensions API, so no code changes are needed for Firefox support.

### Chrome / Chromium

1. Open `chrome://extensions`
2. Enable **Developer mode** (toggle in the top-right corner)
3. Click **Load unpacked** and select the `integrations/extension/` directory

### Firefox

1. Open `about:debugging#/runtime/this-firefox`
2. Click **Load Temporary Add-on...**
3. Select `manifest.json` from the `integrations/extension/` directory

> **Note on background scripts**: Firefox supported `background.scripts` (not `service_worker`) in MV3 until v128, with `service_worker` becoming the default in v133+. The manifest declares both so the extension works across all versions. Chrome ignores `background.scripts`; older Firefox ignores `service_worker`.

Firefox requires a stable `browser_specific_settings.gecko.id` in `manifest.json`, which is already included. Note that temporary add-ons are removed when Firefox restarts; for persistent installation, package the extension as an `.xpi` and install via `about:addons`.

Firefox 113+ is required — this is the `strict_min_version` enforced by `manifest.json`. (MV3 first shipped in Firefox 109, but this extension targets 113 as its minimum.)

### Safari

A Safari Web Extension Xcode project is already checked in at `integrations/apple/` — it references this directory directly, so there is no `safari-web-extension-converter` step:

1. Install Xcode from the Mac App Store
2. Open `integrations/apple/joe-links.xcodeproj` (or run `make ext-safari` from the repo root)
3. Build and run the iOS or macOS scheme (⌘R)
4. Enable the extension in Safari > Settings > Extensions (on macOS you may need **Develop → Allow Unsigned Extensions** first)

After a code change here, just rebuild in Xcode — no conversion or re-import needed.

See SPEC-0008 REQ "Cross-Browser Packaging" and ADR-0012 for architectural context.
