// Package hotwire embeds Turbo and Stimulus, the JavaScript half of Forge's
// Rails-style front end, so an application needs no Node toolchain and no CDN.
//
// The two files are unmodified ES module builds from the npm registry,
// pinned by SHA-256 (see vendor/README.md for provenance and how to update).
// They are served as digested assets and loaded through an import map:
//
//	set, _ := assets.New(assets.Options{}, hotwire.Files(), ...)
//	imports, _ := assets.NewImportMap(set, hotwire.Pins()...)
//
// An application's own JavaScript then says `import "@hotwired/turbo"` and
// `import { Application } from "@hotwired/stimulus"`.
package hotwire

import (
	"embed"
	"io/fs"

	"github.com/rocketinsidecorp/forge-go/assets"
)

// Versions of the vendored packages.
const (
	TurboVersion    = "8.0.23"
	StimulusVersion = "3.2.2"
)

// Lowercase hex SHA-256 of the vendored files. The package test fails when a
// file differs, so an edit or a partial update cannot go unnoticed.
const (
	TurboSHA256    = "b9d35d123a07614f55eaaf993f74d687a503ae41ba50ef835aafa18dbb265a13"
	StimulusSHA256 = "23deecdac6f36c08e8f39fbed6b27f60c850cacad3a34eb71194462b7b1647f3"
)

// TurboProgressBarStyleSHA256 is the base64 SHA-256 of the one inline <style>
// element Turbo 8.0.23 creates (its progress bar). A Content-Security-Policy
// that forbids inline styles blocks it; adding this hash to style-src allows
// exactly that text and nothing else. A test recomputes it from turbo.js.
const TurboProgressBarStyleSHA256 = "WAyOw4V+FqDc35lQPyRADLBWbuNK8ahvYEaQIYF1+Ps="

// Logical asset names of the vendored modules.
const (
	TurboAsset    = "vendor/turbo.js"
	StimulusAsset = "vendor/stimulus.js"
)

// Import map specifiers of the vendored modules.
const (
	TurboSpecifier    = "@hotwired/turbo"
	StimulusSpecifier = "@hotwired/stimulus"
)

//go:embed vendor/turbo.js vendor/stimulus.js
var modules embed.FS

//go:embed vendor/TURBO-LICENSE vendor/STIMULUS-LICENSE
var licenses embed.FS

// Files holds turbo.js and stimulus.js as vendor/turbo.js and
// vendor/stimulus.js, for assets.New.
func Files() fs.FS { return modules }

// Licenses holds the upstream licence texts (vendor/TURBO-LICENSE and
// vendor/STIMULUS-LICENSE). They are not served as assets; the binary carries
// them so the notices travel with the code.
func Licenses() fs.FS { return licenses }

// Pins are the import map pins for Turbo and Stimulus.
func Pins() []assets.Pin {
	return []assets.Pin{
		{Specifier: TurboSpecifier, Logical: TurboAsset},
		{Specifier: StimulusSpecifier, Logical: StimulusAsset},
	}
}

// StyleHashes are the style-src hashes Turbo needs under a strict
// Content-Security-Policy (see TurboProgressBarStyleSHA256), for
// views.Options.StyleHashes.
func StyleHashes() []string { return []string{TurboProgressBarStyleSHA256} }
