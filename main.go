// TODO describe program
package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"flag"
	"fmt"
	"hash/fnv"
	"html/template"
	"image"
	"image/jpeg"
	"io"
	"io/ioutil"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/disintegration/imaging"
	"github.com/rwcarlsen/goexif/exif"
	"github.com/rwcarlsen/goexif/tiff"
	"golang.org/x/sync/errgroup"
)

func main() {
	log.SetFlags(0)
	args := runArgs{
		FullsizeDir: filepath.FromSlash("gallery/fullsize"),
		HTML:        filepath.FromSlash("gallery/index.html"),
		ThumbsDir:   filepath.FromSlash("gallery/thumbnails"),
	}
	flag.StringVar(&args.SrcDir, "source", args.SrcDir, "directory with source jpeg images")
	flag.StringVar(&args.FullsizeDir, "fullsize", args.FullsizeDir, "directory to store full size image copies"+
		" (hardlinked from the source if possible)")
	flag.StringVar(&args.ThumbsDir, "thumbs", args.ThumbsDir, "directory to store thumbnails")
	flag.StringVar(&args.HTML, "html", args.HTML, "generated gallery html file")
	flag.Parse()
	if err := run(args); err != nil {
		log.Fatal(err)
	}
}

type runArgs struct {
	SrcDir      string // source images
	FullsizeDir string // destination directory for full size images
	ThumbsDir   string // generated thumbnails directory
	HTML        string // destination html file
}

func (a *runArgs) validate() error {
	if a.SrcDir == "" {
		return errors.New("source directory must be set")
	}
	if a.FullsizeDir == "" {
		return errors.New("destination directory must be set")
	}
	if a.ThumbsDir == "" {
		return errors.New("thumbnails directory must be set")
	}
	if a.HTML == "" {
		return errors.New("output html file must be set")
	}
	if a.FullsizeDir == a.ThumbsDir {
		return errors.New("destination and thumbnail directories cannot be the same")
	}
	if a.SrcDir == a.ThumbsDir {
		return errors.New("source and thumbnail directories cannot be the same")
	}
	if dir, _ := filepath.Split(a.HTML); dir != "" {
		if !strings.HasPrefix(a.ThumbsDir, dir) {
			return errors.New("thumbnails directory cannot be above html file in FS hierarchy")
		}
		if !strings.HasPrefix(a.FullsizeDir, dir) {
			return errors.New("destination directory cannot be above html file in FS hierarchy")
		}
	}
	return nil
}

func run(args runArgs) error {
	if err := args.validate(); err != nil {
		return err
	}
	if err := os.MkdirAll(args.ThumbsDir, 0777); err != nil {
		return err
	}
	if err := os.MkdirAll(args.FullsizeDir, 0777); err != nil {
		return err
	}
	tr, err := newTransform(0, 0, 500, 500)
	if err != nil {
		panic(err)
	}
	var mu sync.Mutex // protects concurrent population of galleryImages
	var galleryImages []imageDetails

	workers := runtime.GOMAXPROCS(0)
	if workers < 1 {
		workers = 1
	}
	ch := make(chan string)
	group, ctx := errgroup.WithContext(context.Background())
	for i := 0; i < workers; i++ {
		group.Go(func() error {
			for p := range ch {
				base := filepath.Base(p)
				fullsizeImage := filepath.Join(args.FullsizeDir, base)
				thumbnailFile := filepath.Join(args.ThumbsDir, base)
				details := imageDetails{
					Original:  filepath.ToSlash(fullsizeImage),
					Thumbnail: filepath.ToSlash(thumbnailFile),
				}
				if dir := filepath.Dir(args.HTML); dir != "" {
					s, err := filepath.Rel(dir, fullsizeImage)
					if err != nil {
						return err
					}
					details.Original = filepath.ToSlash(s)
					s, err = filepath.Rel(dir, thumbnailFile)
					if err != nil {
						return err
					}
					details.Thumbnail = filepath.ToSlash(s)
				}
				if id, err := imageHash(p); err != nil {
					return err
				} else {
					details.ID = id
				}
				if err := createThumbnail(tr, thumbnailFile, p); err != nil {
					return err
				}
				if err := linkOrCopy(fullsizeImage, p); err != nil {
					return err
				}
				// TODO: maybe move isPortrait check into thumbnail generation?
				if ok, err := isPortrait(thumbnailFile); err != nil {
					return err
				} else {
					details.Portrait = ok
				}
				var err error
				if details.Time, err = imageTime(p); err != nil {
					return err
				}
				mu.Lock()
				galleryImages = append(galleryImages, details)
				mu.Unlock()
			}
			return nil
		})
	}
	group.Go(func() error {
		defer close(ch)
		ticker := time.NewTicker(2 * time.Second)
		defer ticker.Stop()
		var n int
		walkFunc := func(p string, info os.FileInfo, err error) error {
			if err != nil {
				return err
			}
			if p == args.ThumbsDir || p == args.FullsizeDir {
				return filepath.SkipDir
			}
			ext := filepath.Ext(p)
			if !info.Mode().IsRegular() || !(strings.EqualFold(ext, ".jpg") || strings.EqualFold(ext, ".jpeg")) {
				return nil
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case ch <- p:
				n++
			}
			select {
			case <-ticker.C:
				log.Printf("processed %d images", n)
			default:
			}
			return nil
		}
		return filepath.Walk(args.SrcDir, walkFunc)
	})
	if err := group.Wait(); err != nil {
		return err
	}
	if len(galleryImages) == 0 {
		return errors.New("no images found")
	}
	sort.Slice(galleryImages, func(i, j int) bool {
		return galleryImages[i].Time.After(galleryImages[j].Time)
	})
	for _, d := range galleryImages {
		fmt.Println(d)
	}
	buf := new(bytes.Buffer)
	if err := gallery.Execute(buf, galleryImages); err != nil {
		return err
	}
	return ioutil.WriteFile(args.HTML, buf.Bytes(), 0666)
}

type imageDetails struct {
	Portrait  bool      // whether image height is larger than width
	Original  string    // path to original image
	Thumbnail string    // thumbnail
	ID        string    // image id (TODO: use phash here)
	Time      time.Time // either date from exif or mtime
}

// isPortrait reports whether image is in a portrait orientation (its height is
// larger than width). It does not take EXIF rotation into account.
func isPortrait(name string) (bool, error) {
	f, err := os.Open(name)
	if err != nil {
		return false, err
	}
	defer f.Close()
	cfg, err := jpeg.DecodeConfig(f)
	if err != nil {
		return false, err
	}
	return cfg.Height > cfg.Width, nil
}

func createThumbnail(tr transform, dst, src string) error {
	thumb, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0666)
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			return nil
		}
		return err
	}
	var defuse bool
	defer func() {
		if defuse {
			return
		}
		_ = os.Remove(dst)
	}()
	defer thumb.Close()

	f, err := os.Open(src)
	if err != nil {
		return err
	}
	defer f.Close()

	exifMeta, err := exif.Decode(f)
	if err != nil {
		return fmt.Errorf("exif decode: %w", err)
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return err
	}

	img, err := jpeg.Decode(f)
	if err != nil {
		return err
	}

	w, h := img.Bounds().Dx(), img.Bounds().Dy()

	if exifMeta != nil {
		rotate, swapWH := useExifOrientation(exifMeta)
		if swapWH {
			w, h = h, w
		}
		if rotate != nil {
			img = rotate(img)
		}
	}

	if w, h, err = tr.newDimensions(w, h); err != nil {
		return err
	}
	img, err = resizeImage(img, w, h)
	if err != nil {
		return err
	}
	if err = jpeg.Encode(thumb, img, &jpeg.Options{Quality: 95}); err != nil {
		return err
	}
	if err = thumb.Close(); err != nil {
		return err
	}
	defuse = true
	return nil
}

// linkOrCopy creates a copy of a source file at its destination. It first
// checks whether dst already existst and returns nil right away if it does. If
// it does not exist, it tries to create a hard link. If that fails, it copies
// file.
func linkOrCopy(dst, src string) error {
	if _, err := os.Stat(dst); err == nil {
		return nil
	}
	if err := os.Link(src, dst); err == nil {
		return nil
	}
	f, err := os.Open(src)
	if err != nil {
		return err
	}
	defer f.Close()
	f2, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0666)
	if err != nil {
		return err
	}
	defer f2.Close()
	if _, err := io.Copy(f2, f); err != nil {
		_ = os.Remove(f2.Name())
		return err
	}
	return f2.Close()
}

func imageHash(s string) (string, error) {
	f, err := os.Open(s)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := fnv.New64a()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return base64.RawStdEncoding.EncodeToString(h.Sum(nil)), nil
}

func useExifOrientation(meta *exif.Exif) (rotatefunc func(image.Image) image.Image, swapWH bool) {
	o, err := meta.Get(exif.Orientation)
	if err != nil || o == nil || len(o.Val) != 2 {
		return nil, false
	}
	for _, x := range o.Val {
		switch x {
		case 3: // 180º
			return rotate180, false
		case 6: // 90ºCCW
			return rotate90ccw, true
		case 8: // 90ºCW
			return rotate90cw, true
		case 4: // vertical flip
			return flipVertical, true
		case 2: // horizontal flip
			return flipHorizontal, true
		}
	}
	return nil, false
}

func flipHorizontal(src image.Image) image.Image { return imaging.FlipH(src) }
func flipVertical(src image.Image) image.Image   { return imaging.FlipV(src) }
func rotate90ccw(src image.Image) image.Image    { return imaging.Rotate270(src) }
func rotate90cw(src image.Image) image.Image     { return imaging.Rotate90(src) }
func rotate180(src image.Image) image.Image      { return imaging.Rotate180(src) }

// imageTime returns either time from EXIF metadata, or mtime of the file
func imageTime(name string) (time.Time, error) {
	f, err := os.Open(name)
	if err != nil {
		return time.Time{}, err
	}
	defer f.Close()
	if meta, err := exif.Decode(f); err == nil {
		if t, err := meta.DateTime(); err == nil && !t.IsZero() {
			return t.UTC(), nil
		}
		if t, err := dateTimeDigitized(meta); err == nil && !t.IsZero() {
			return t.UTC(), nil
		}
	}
	fi, err := f.Stat()
	if err != nil {
		return time.Time{}, err
	}
	return fi.ModTime().UTC(), nil
}

// dateTimeDigitized is a copy of exif.EXIF.DateTime method, but it looks at a
// DateTimeDigitized tag instead
func dateTimeDigitized(x *exif.Exif) (time.Time, error) {
	var dt time.Time
	tag, err := x.Get(exif.DateTimeDigitized)
	if err != nil {
		return dt, err

	}
	if tag.Format() != tiff.StringVal {
		return dt, errors.New("DateTimeDigitized not in string format")
	}
	const exifTimeLayout = "2006:01:02 15:04:05"
	dateStr := strings.TrimRight(string(tag.Val), "\x00")
	timeZone := time.Local
	if tz, _ := x.TimeZone(); tz != nil {
		timeZone = tz
	}
	return time.ParseInLocation(exifTimeLayout, dateStr, timeZone)
}

func resizeImage(img image.Image, width, height int) (image.Image, error) {
	return imaging.Resize(img, width, height, imaging.CatmullRom), nil
}

type transform struct {
	Width     int
	Height    int
	MaxWidth  int
	MaxHeight int
}

func (tr transform) newDimensions(origWidth, origHeight int) (width, height int, err error) {
	if origWidth == 0 || origHeight == 0 {
		return 0, 0, errors.New("invalid source dimensions")
	}
	var w, h int
	switch {
	case tr.MaxWidth > 0 || tr.MaxHeight > 0:
		w, h = tr.MaxWidth, tr.MaxHeight
		// if only one max dimension specified, calculate another using
		// original aspect ratio
		if w == 0 {
			w = origWidth * h / origHeight
		}
		if h == 0 {
			h = origHeight * w / origWidth
		}
		if origWidth <= w && origHeight <= h {
			return origWidth, origHeight, nil // image already fit
		}
		if tr.MaxWidth > 0 && tr.MaxHeight > 0 {
			// maxwidth and maxheight form free aspect ratio, need
			// to adjust w and h to match origin aspect ratio, while
			// keeping dimensions inside max bounds
			if float64(origWidth)/float64(origHeight) > float64(w)/float64(h) {
				h = origHeight * w / origWidth
			} else {
				w = origWidth * h / origHeight
			}
		}
	case tr.Width > 0 || tr.Height > 0:
		// if both width and height specified, free aspect ratio is
		// applied; if only one is set, original aspect ratio is kept
		w, h = tr.Width, tr.Height
		if w == 0 {
			w = origWidth * h / origHeight
		}
		if h == 0 {
			h = origHeight * w / origWidth
		}
	default:
		return 0, 0, fmt.Errorf("invalid transform %v", tr)
	}
	// if w*h > pixelLimit || w >= 1<<16 || h >= 1<<16 {
	// 	return 0, 0, errors.New("destination size exceeds limit")
	// }
	return w, h, nil
}

func newTransform(width, height, maxWidth, maxHeight int) (transform, error) {
	tr := transform{
		Width:     width,
		Height:    height,
		MaxWidth:  maxWidth,
		MaxHeight: maxHeight,
	}
	if tr.Width == 0 && tr.Height == 0 && tr.MaxWidth == 0 && tr.MaxHeight == 0 {
		return transform{}, errors.New("no valid dimensions specified")
	}
	// if tr.Width*tr.Height > pixelLimit || tr.MaxWidth > pixelLimit || tr.MaxHeight > pixelLimit {
	// 	return transform{}, errors.New("destination size exceeds limit")
	// }
	return tr, nil
}

var gallery = template.Must(template.New("gallery").Parse(`<!DOCTYPE html><head><title>Gallery</title>
<meta charset="utf-8">
<style>
	* {box-sizing: border-box; border: none;}
	html {background-color: whitesmoke; padding:0;margin:0;}
	body {padding:0;margin:0;}
	.gallery {
        display: grid;
        grid-template-columns: repeat(auto-fit, minmax(300px, 1fr));
        grid-gap: 5px;
        grid-auto-flow: row dense;

        padding: 5px;
        margin: auto;
    }
    .gallery .portrait {
        grid-row-end: span 2;
    }
    .gallery img {
        display: block;
        object-fit: cover;
        width: 100%;
		height: 100%;
    }
    figure {
        padding: 0;
        margin: 0;
    }
    .lightbox {
        display: none;
    }
    .lightbox:target {
        z-index: 999;
        outline: none;
        display: block;
        position: fixed;
        top: 0;
        left: 0;
        width: 100%;
        height: 100vh;
        background-color: rgba(0, 0, 0, 0.9);
    }
    .lightbox:target img {
        object-fit: scale-down;
        width: 100%;
        height: 100%;
    }
</style>
</head>
<body>
<div class="gallery">
{{range .}}
	<figure{{if .Portrait}} class="portrait"{{end}}><a href="#{{.ID}}">
	<img loading="lazy" src="{{.Thumbnail}}">
	</a>
	</figure>
{{end}}
</div>
<div class="fullsize-images">
{{range .}}
	<figure class="lightbox" id="{{.ID}}">
		<a href="#back">
		<img loading="lazy" src="{{.Original}}">
		</a>
	</figure>
{{end}}
</div>
</body>
`))
