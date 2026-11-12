package core

import "fmt"

const MaxImagePixels = 20_000_000

func ValidateImageDimensions(width, height int) error {
	if width <= 0 || height <= 0 || width > MaxImagePixels/height {
		return fmt.Errorf("image dimensions exceed %d pixel limit", MaxImagePixels)
	}
	return nil
}
