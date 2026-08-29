// Package manifest renders the itms-services plist iOS reads to install a build.
//
// All URLs in it must be absolute, which is why the service needs to know its own
// external base URL (DESIGN §8).
package manifest

import (
	"net/url"

	"howett.net/plist"
)

type asset struct {
	Kind string `plist:"kind"`
	URL  string `plist:"url"`
}

type metadata struct {
	BundleIdentifier string `plist:"bundle-identifier"`
	BundleVersion    string `plist:"bundle-version"`
	Kind             string `plist:"kind"`
	Title            string `plist:"title"`
}

type item struct {
	Assets   []asset  `plist:"assets"`
	Metadata metadata `plist:"metadata"`
}

type document struct {
	Items []item `plist:"items"`
}

// Render builds the manifest for one build.
//
// bundleID must be the identifier read out of the IPA itself: if it disagrees with what
// the archive actually contains, the install fails on the device with a message that does
// not explain why (DESIGN §8).
//
// The document is marshalled from a struct rather than templated, so a title containing
// markup characters cannot break the plist.
func Render(bundleID, version, title, ipaURL string) ([]byte, error) {
	doc := document{Items: []item{{
		Assets: []asset{{Kind: "software-package", URL: ipaURL}},
		Metadata: metadata{
			BundleIdentifier: bundleID,
			BundleVersion:    version,
			Kind:             "software",
			Title:            title,
		},
	}}}
	return plist.MarshalIndent(doc, plist.XMLFormat, "\t")
}

// InstallURL is the itms-services URL an install button targets. iOS hands the manifest
// URL to its install daemon, which fetches it over HTTPS itself.
func InstallURL(manifestURL string) string {
	return "itms-services://?action=download-manifest&url=" + url.QueryEscape(manifestURL)
}
