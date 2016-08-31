package vips

/*
#cgo pkg-config: vips
#include "vips.h"
*/
import "C"

import (
	"bytes"
	"errors"
	"fmt"
	"math"
	"runtime"
	"strings"
	"unsafe"
)

var (
	MARKER_JPEG = []byte{0xff, 0xd8}
	MARKER_PNG  = []byte{0x89, 0x50}
)

type ImageType int

const (
	UNKNOWN ImageType = iota
	JPEG
	PNG
)

type Interpolator int

const (
	BICUBIC Interpolator = iota
	BILINEAR
	NOHALO
)

type Extend int

const (
	EXTEND_BLACK Extend = C.VIPS_EXTEND_BLACK
	EXTEND_WHITE Extend = C.VIPS_EXTEND_WHITE
)

var interpolations = map[Interpolator]string{
	BICUBIC:  "bicubic",
	BILINEAR: "bilinear",
	NOHALO:   "nohalo",
}

func (i Interpolator) String() string { return interpolations[i] }

type CropRect struct {
	Top    uint
	Left   uint
	Width  uint
	Height uint
}

type Options struct {
	Height       int
	Width        int
	Crop         bool // Deprecated
	CropRect     *CropRect
	Enlarge      bool
	Extend       Extend
	Embed        bool
	Interpolator Interpolator
	BlurAmount   float32
	Gravity      Gravity
	Quality      int
}

type VipsImagePtr *C.struct__VipsImage

func init() {
	Initialize()
}

var initialized bool

func Initialize() {
	if initialized {
		return
	}

	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	if err := C.vips_initialize(); err != 0 {
		C.vips_shutdown()
		panic("unable to start vips!")
	}

	C.vips_concurrency_set(1)
	C.vips_cache_set_max_mem(100 * 1048576) // 100Mb
	C.vips_cache_set_max(500)

	initialized = true
}

func Shutdown() {
	if !initialized {
		return
	}

	C.vips_shutdown()

	initialized = false
}

func Debug() {
	C.im__print_all()
}

func Crop(image VipsImagePtr, top uint, left uint, width uint, height uint) (VipsImagePtr, error) {
	var outImage *C.struct__VipsImage

	C.vips_crop_(image, &outImage, C.int(top), C.int(left), C.int(width), C.int(height))

	if outImage != nil {
		return VipsImagePtr(outImage), nil
	} else {
		C.vips_error_freeze()
		errStr := C.GoString(C.vips_error_buffer())
		errStr = strings.TrimRight(errStr, " \n")
		C.vips_error_clear()
		C.vips_error_thaw()
		return nil, errors.New(fmt.Sprintf("Could not crop image: %s", errStr))
	}
}

func validCrop(image VipsImagePtr, crop *CropRect) bool {
	if crop == nil {
		return false
	}

	imageWidth := uint(image.Xsize)
	imageHeight := uint(image.Ysize)

	return imageWidth >= crop.Left+crop.Width &&
		imageHeight >= crop.Top+crop.Height
}

func validateCrop(image VipsImagePtr, crop *CropRect) *CropRect {
	if crop == nil {
		return nil
	}

	imageWidth := uint(image.Xsize)
	imageHeight := uint(image.Ysize)

	if crop.Top > imageHeight || crop.Left > imageWidth {
		return nil
	}

	if crop.Left+crop.Width > imageWidth {
		crop.Width = imageWidth - crop.Left
	}

	if crop.Top+crop.Height > imageHeight {
		crop.Height = imageHeight - crop.Top
	}

	return crop
}

func ResizeMagick(buf []byte, o Options) ([]byte, error) {
	var image, tmpImage *C.struct__VipsImage

	C.vips_magickload_buffer_(unsafe.Pointer(&buf[0]), C.size_t(len(buf)), &image)

	// TODO: Consider not doing this on _every_ image
	defer C.vips_thread_shutdown()

	if image == nil {
		return nil, errors.New("unknown image format")
	}

	cropRect := validateCrop(image, o.CropRect)

	if cropRect != nil {
		tmpImage, err := Crop(image, cropRect.Left, cropRect.Top, cropRect.Width, cropRect.Height)

		if err != nil {
			C.g_object_unref(C.gpointer(image))
			return nil, err
		}

		C.g_object_unref(C.gpointer(image))
		image = tmpImage
	}

	// get WxH
	inWidth := int(image.Xsize)
	inHeight := int(image.Ysize)

	// prepare for factor
	factor := 0.0

	switch {
	// Fixed width and height
	case o.Width > 0 && o.Height > 0:
		xf := float64(inWidth) / float64(o.Width)
		yf := float64(inHeight) / float64(o.Height)
		factor = math.Max(xf, yf)
	// Fixed width, auto height
	case o.Width > 0:
		factor = float64(inWidth) / float64(o.Width)
		o.Height = int(math.Floor(float64(inHeight) / factor))
	// Fixed height, auto width
	case o.Height > 0:
		factor = float64(inHeight) / float64(o.Height)
		o.Width = int(math.Floor(float64(inWidth) / factor))
	// Identity transform
	default:
		factor = 1
		o.Width = inWidth
		o.Height = inHeight
	}

	// shrink
	shrink := int(math.Floor(factor))
	if shrink < 1 {
		shrink = 1
	}

	// residual
	residual := float64(shrink) / factor

	// Do not enlarge the output if the input width *or* height are already less than the required dimensions
	if !o.Enlarge {
		if inWidth < o.Width && inHeight < o.Height {
			factor = 1
			shrink = 1
			residual = 0
			o.Width = inWidth
			o.Height = inHeight
		}
	}

	if shrink > 1 {
		// Use vips_shrink with the integral reduction
		err := C.vips_shrink_0(image, &tmpImage, C.double(float64(shrink)), C.double(float64(shrink)))
		C.g_object_unref(C.gpointer(image))
		image = tmpImage
		if err != 0 {
			return nil, resizeError()
		}

		// Recalculate residual float based on dimensions of required vs shrunk images
		shrunkWidth := int(image.Xsize)
		shrunkHeight := int(image.Ysize)

		residualx := float64(o.Width) / float64(shrunkWidth)
		residualy := float64(o.Height) / float64(shrunkHeight)
		residual = math.Min(residualx, residualy)
	}

	// Use vips_affine with the remaining float part
	if residual != 0 {
		// Create interpolator - "bilinear" (default), "bicubic" or "nohalo"
		is := C.CString(o.Interpolator.String())
		interpolator := C.vips_interpolate_new(is)

		// Perform affine transformation
		err := C.vips_affine_interpolator(image, &tmpImage, C.double(residual), 0, 0, C.double(residual), interpolator)
		C.g_object_unref(C.gpointer(image))
		C.g_object_unref(C.gpointer(interpolator))
		C.free(unsafe.Pointer(is))
		image = tmpImage
		if err != 0 {
			return nil, resizeError()
		}
	}

	// Always flatten
	if image.Bands > 3 {
		if -1 != C.vips_flatten_0(image, &tmpImage) {
			C.g_object_unref(C.gpointer(image))
			image = tmpImage
		}
	}

	// Always convert to sRGB colour space
	if -1 != C.vips_colourspace_0(image, &tmpImage, C.VIPS_INTERPRETATION_sRGB) {
		C.g_object_unref(C.gpointer(image))
		image = tmpImage
	}

	// Apply blur if needed
	if o.BlurAmount > 0 {
		if -1 != C.vips_gaussian_blur(image, &tmpImage, C.double(o.BlurAmount)) {
			C.g_object_unref(C.gpointer(image))
			image = tmpImage
		}
	}

	// Finally save
	length := C.size_t(0)
	var ptr unsafe.Pointer

	C.vips_jpegsave_custom(image, &ptr, &length, 1, C.int(o.Quality), 0)
	C.g_object_unref(C.gpointer(image))

	// get back the buffer
	buf = C.GoBytes(ptr, C.int(length))

	// cleanup
	C.g_free(C.gpointer(ptr))
	C.vips_error_clear()

	return buf, nil
}

func Resize(buf []byte, o Options) ([]byte, error) {
	// detect (if possible) the file type
	typ := UNKNOWN
	switch {
	case bytes.Equal(buf[:2], MARKER_JPEG):
		typ = JPEG
	case bytes.Equal(buf[:2], MARKER_PNG):
		typ = PNG
	default:
		return nil, errors.New("unknown image format")
	}

	// create an image instance
	var image, tmpImage *C.struct__VipsImage

	// Do shrink on load by default, however
	// don't do it in the case of cropped images
	useShrinkOnLoad := true

	// feed it
	switch typ {
	case JPEG:
		C.vips_jpegload_buffer_seq(unsafe.Pointer(&buf[0]), C.size_t(len(buf)), &image)
	case PNG:
		C.vips_pngload_buffer_seq(unsafe.Pointer(&buf[0]), C.size_t(len(buf)), &image)
	}

	// cleanup
	defer func() {
		C.vips_thread_shutdown()
		C.vips_error_clear()
	}()

	// defaults
	if o.Quality == 0 {
		o.Quality = 100
	}

	if image == nil {
		return nil, errors.New("unknown image format")
	}

	cropRect := validateCrop(image, o.CropRect)

	if cropRect != nil {
		tmpImage, err := Crop(image, cropRect.Left, cropRect.Top, cropRect.Width, cropRect.Height)

		if err != nil {
			C.g_object_unref(C.gpointer(image))
			return nil, err
		}

		C.g_object_unref(C.gpointer(image))

		// We've cropped the image, no longer safe to do shrinkOnLoad
		useShrinkOnLoad = false
		image = tmpImage
	}

	// get WxH
	inWidth := int(image.Xsize)
	inHeight := int(image.Ysize)

	// prepare for factor
	factor := 0.0

	// image calculations
	switch {
	// Fixed width and height
	case o.Width > 0 && o.Height > 0:
		xf := float64(inWidth) / float64(o.Width)
		yf := float64(inHeight) / float64(o.Height)
		if o.Crop {
			factor = math.Min(xf, yf)
		} else {
			factor = math.Max(xf, yf)
		}
	// Fixed width, auto height
	case o.Width > 0:
		factor = float64(inWidth) / float64(o.Width)
		o.Height = int(math.Floor(float64(inHeight) / factor))
	// Fixed height, auto width
	case o.Height > 0:
		factor = float64(inHeight) / float64(o.Height)
		o.Width = int(math.Floor(float64(inWidth) / factor))
	// Identity transform
	default:
		factor = 1
		o.Width = inWidth
		o.Height = inHeight
	}

	// shrink
	shrink := int(math.Floor(factor))
	if shrink < 1 {
		shrink = 1
	}

	// residual
	residual := float64(shrink) / factor

	// Do not enlarge the output if the input width *or* height are already less than the required dimensions
	if !o.Enlarge {
		if inWidth < o.Width && inHeight < o.Height {
			factor = 1
			shrink = 1
			residual = 0
			o.Width = inWidth
			o.Height = inHeight
		}
	}

	// Try to use libjpeg shrink-on-load
	shrinkOnLoad := 1
	if typ == JPEG && shrink >= 2 {
		switch {
		case shrink >= 8:
			factor = factor / 8
			shrinkOnLoad = 8
		case shrink >= 4:
			factor = factor / 4
			shrinkOnLoad = 4
		case shrink >= 2:
			factor = factor / 2
			shrinkOnLoad = 2
		}
	}

	if useShrinkOnLoad && shrinkOnLoad > 1 {
		// Recalculate integral shrink and double residual
		factor = math.Max(factor, 1.0)
		shrink = int(math.Floor(factor))
		residual = float64(shrink) / factor
		// Reload input using shrink-on-load
		err := C.vips_jpegload_buffer_shrink(unsafe.Pointer(&buf[0]), C.size_t(len(buf)), &tmpImage, C.int(shrinkOnLoad))
		C.g_object_unref(C.gpointer(image))
		image = tmpImage
		if err != 0 {
			return nil, resizeError()
		}
	}

	if shrink > 1 {
		// Use vips_shrink with the integral reduction
		err := C.vips_shrink_0(image, &tmpImage, C.double(float64(shrink)), C.double(float64(shrink)))
		C.g_object_unref(C.gpointer(image))
		image = tmpImage
		if err != 0 {
			return nil, resizeError()
		}

		// Recalculate residual float based on dimensions of required vs shrunk images
		shrunkWidth := int(image.Xsize)
		shrunkHeight := int(image.Ysize)

		residualx := float64(o.Width) / float64(shrunkWidth)
		residualy := float64(o.Height) / float64(shrunkHeight)
		residual = math.Min(residualx, residualy)
	}

	// Use vips_affine with the remaining float part
	if residual != 0 {
		// Create interpolator - "bilinear" (default), "bicubic" or "nohalo"
		is := C.CString(o.Interpolator.String())
		interpolator := C.vips_interpolate_new(is)

		// Perform affine transformation
		err := C.vips_affine_interpolator(image, &tmpImage, C.double(residual), 0, 0, C.double(residual), interpolator)
		C.g_object_unref(C.gpointer(image))

		image = tmpImage

		C.free(unsafe.Pointer(is))
		C.g_object_unref(C.gpointer(interpolator))

		if err != 0 {
			return nil, resizeError()
		}
	}

	// Switch to sRGB before we do anything else because flattening
	// with some other colorspece will not bode well in most other cases
	// Always convert to sRGB colour space
	if -1 != C.vips_colourspace_0(image, &tmpImage, C.VIPS_INTERPRETATION_sRGB) {
		C.g_object_unref(C.gpointer(image))
		image = tmpImage
	}

	// Only flatten if we're not running CMYK and we're rocking
	// more than the standard 3 bands
	if image.Type != C.VIPS_INTERPRETATION_CMYK && image.Bands > 3 {
		if -1 != C.vips_flatten_0(image, &tmpImage) {
			C.g_object_unref(C.gpointer(image))
			image = tmpImage
		}
	}

	// Apply blur if needed
	if o.BlurAmount > 0 {
		if -1 != C.vips_gaussian_blur(image, &tmpImage, C.double(o.BlurAmount)) {
			C.g_object_unref(C.gpointer(image))
			image = tmpImage
		}
	}

	// Finally save
	length := C.size_t(0)
	var ptr unsafe.Pointer
	C.vips_jpegsave_custom(image, &ptr, &length, 1, C.int(o.Quality), 0)
	C.g_object_unref(C.gpointer(image))

	// get back the buffer
	buf = C.GoBytes(ptr, C.int(length))
	C.g_free(C.gpointer(ptr))

	return buf, nil
}

func resizeError() error {
	s := C.GoString(C.vips_error_buffer())
	C.vips_error_clear()
	return errors.New(s)
}

type Gravity int

const (
	CENTRE Gravity = 1 << iota
	NORTH
	EAST
	SOUTH
	WEST
)

func sharpCalcCrop(inWidth, inHeight, outWidth, outHeight int, gravity Gravity) (int, int) {
	left := (inWidth - outWidth + 1) / 2
	top := (inHeight - outHeight + 1) / 2

	if (gravity & NORTH) != 0 {
		top = 0
	}

	if (gravity & EAST) != 0 {
		left = inWidth - outWidth
	}

	if (gravity & SOUTH) != 0 {
		top = inHeight - outHeight
	}

	if (gravity & WEST) != 0 {
		left = 0
	}

	return left, top
}
