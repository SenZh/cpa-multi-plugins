// image_input.go — image-input gate for the qoder channel (issue #37 follow-up).
//
// History. v0.8.66 (0d50f05) added an unconditional rejection of image_url
// parts: the plugin authors tested the Intl realm, saw the image never reach
// the model, and turned the silent drop into an honest error
// (body_vision_test.go). The rejection was a *fallback*, not a protocol
// verdict: image_upload.go in the same tree still carries the full OSS
// upload implementation plus the note that the CN gateway
// (gateway.qoder.com.cn) had never been tested against that dialect.
//
// 2026-10-20 live matrix on the CN realm (this deployment, account
// qoder-cn-01a14d52…, model qfmodel) settles it — the plugin's existing
// messages passthrough already delivers pixels:
//
//	dialect A  messages content [{text},{image_url}] inline data URI  -> read back "ZX-4417"
//	dialect C  no chat_context at all                                 -> "Red"
//	dialect D  official template contents:[…]                         -> "Red"
//	dialect E  image only, no text part, empty chat_context.text      -> described the image
//	1.6MB base64 screenshot                                           -> quoted the on-screen error code
//	history image + text follow-up                                    -> clean answer
//
// So the gate is a deployment switch, not a capability verdict:
//
//	image_forward: true   (default) forward image parts verbatim
//	image_forward: false            keep the v0.8.66 honest rejection
//
// Set it to false if you run the Intl realm and images silently do nothing
// there — a wrong answer is worse than a loud refusal, which is exactly the
// trade issue #37 asked for.
package main

import "sync"

// defaultImageForward is the shipped default: forward image parts. Realms
// where the image is silently ignored must opt out with image_forward: false.
const defaultImageForward = true

var (
	imageForwardMu sync.RWMutex
	imageForward   = defaultImageForward
)

// imageForwardEnabled reports whether image_url parts may be forwarded.
func imageForwardEnabled() bool {
	imageForwardMu.RLock()
	defer imageForwardMu.RUnlock()
	return imageForward
}

// setImageForward flips the gate (configure + tests).
func setImageForward(on bool) {
	imageForwardMu.Lock()
	imageForward = on
	imageForwardMu.Unlock()
}
