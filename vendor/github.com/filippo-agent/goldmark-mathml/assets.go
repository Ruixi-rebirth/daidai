package mathml

import (
	"embed"
	"io/fs"
)

//go:embed assets
var assets embed.FS

// Assets returns the Temml stylesheet and its small supplemental font as an
// fs.FS rooted at the assets directory. Web applications can serve it with
// http.FileServerFS. The stylesheet is optional for basic MathML rendering but
// works around browser differences and improves accents and script glyphs.
func Assets() fs.FS {
	result, err := fs.Sub(assets, "assets")
	if err != nil {
		panic(err) // The embedded directory is fixed at build time.
	}
	return result
}
