package assets

import "embed"

// SmallFaces contains the bundled face stickers used as startup defaults.
//
//go:embed daidai_faces_small/*.png
var SmallFaces embed.FS
