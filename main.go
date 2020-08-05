// Command photo-gallery is a simple web photo gallery generator.
//
// It takes a directory with jpeg images (.jpg or .jpeg suffixes) and produces
// HTML file along with two directories: one holds full-sized copies of
// original photos, another contains thumbnails. These directories + an HTML
// file are compatible with any web server supporting static content.
//
// The default template produces a self-contained gallery using only HTML and
// CSS.
package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
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

	"github.com/artyom/phash"
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
	flag.StringVar(&args.SrcDir, "src", args.SrcDir, "`directory` with source jpeg images")
	flag.StringVar(&args.FullsizeDir, "orig", args.FullsizeDir, "`directory` to store full size image copies"+
		" (hardlinked from the source if possible)")
	flag.StringVar(&args.ThumbsDir, "thumb", args.ThumbsDir, "`directory` to store thumbnails")
	flag.StringVar(&args.HTML, "html", args.HTML, "generated gallery html `file`")
	flag.StringVar(&args.Template, "template", args.Template, "template `file` to use instead of default")
	flag.StringVar(&args.Name, "name", args.Name, "optional gallery name")
	flag.StringVar(&args.Cache, "cache", args.Cache, "optional metadata cache `file`, enables incremental gallery update")
	flag.BoolVar(&args.Phash, "phash", args.Phash, "use perceptual hash to detect duplicates on add (slow)")

	var dump bool
	flag.BoolVar(&dump, "dumptemplate", dump, "dump default template to stdout and exit")
	flag.Parse()
	if dump {
		fmt.Println(defaultTemplateBody)
		return
	}
	if err := run(args); err != nil {
		log.Fatal(err)
	}
}

type runArgs struct {
	SrcDir      string // source images
	FullsizeDir string // destination directory for full size images
	ThumbsDir   string // generated thumbnails directory
	HTML        string // destination html file

	Template string // optional template file to override default
	Cache    string // optional gallery metadata cache
	Name     string // optional gallery name
	Phash    bool   // whether to use (slower) perceptual image hash
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
	gallery := defaultTemplate
	if args.Template != "" {
		var err error
		if gallery, err = template.ParseFiles(args.Template); err != nil {
			return err
		}
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
	page := &galleryCache{Name: "Gallery", UsePhash: args.Phash}
	if args.Cache != "" {
		switch c, err := loadCache(args.Cache); {
		case os.IsNotExist(err):
		case err != nil:
			return err
		default:
			if c.UsePhash != page.UsePhash {
				log.Printf("metadata cache stored with -phash=%v, using it", c.UsePhash)
			}
			page = c
		}
	}
	if args.Name != "" {
		page.Name = args.Name
	}
	workers := runtime.GOMAXPROCS(0)
	if workers < 1 {
		workers = 1
	}
	ch := make(chan string)
	group, ctx := errgroup.WithContext(context.Background())
	for i := 0; i < workers; i++ {
		group.Go(func() error {
			for p := range ch {
				var id uint64
				var err error
				if page.UsePhash {
					id, err = imagePhash(p)
				} else {
					id, err = fileHash(p)
				}
				if err != nil {
					return err
				}
				fullsizeImage := filepath.Join(args.FullsizeDir, fmt.Sprintf("%x%s", id, filepath.Ext(p)))
				thumbnailFile := filepath.Join(args.ThumbsDir, fmt.Sprintf("%x.jpg", id))
				details := imageDetails{
					Original:  filepath.ToSlash(fullsizeImage),
					Thumbnail: filepath.ToSlash(thumbnailFile),
					Source:    p,
					Hash:      id,
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
				if details.Time, err = imageTime(p); err != nil {
					return err
				}
				if err := page.add(details); err != nil {
					return fmt.Errorf("adding %q: %w", p, err)
				}
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
	if len(page.Images) == 0 {
		return errors.New("no images found")
	}
	page.sortByTime()
	buf := new(bytes.Buffer)
	if err := gallery.Execute(buf, page); err != nil {
		return err
	}
	if err := ioutil.WriteFile(args.HTML, buf.Bytes(), 0666); err != nil {
		return err
	}
	log.Printf("images added: %d, total: %d", page.n, len(page.Images))
	if args.Cache != "" {
		return saveCache(page, args.Cache)
	}
	return nil
}

type imageDetails struct {
	Portrait  bool      `json:",omitempty"` // whether image height is larger than width
	Original  string    // full-sized image copy
	Thumbnail string    // thumbnail
	Source    string    // source file name (OS and filesystem-specific)
	Hash      uint64    `json:",string"`
	Time      time.Time // either date from exif or mtime
}

// idToBytes returns v as byte slice laid out in big-endian order
func idToBytes(v uint64) []byte {
	var b []byte
	return append(b, byte(v>>56), byte(v>>48), byte(v>>40), byte(v>>32), byte(v>>24), byte(v>>16), byte(v>>8), byte(v))
}

func (d *imageDetails) ID() string {
	return base64.RawURLEncoding.EncodeToString(idToBytes(d.Hash))
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

	img, err := imaging.Decode(f, imaging.AutoOrientation(true))
	if err != nil {
		return err
	}
	w, h := img.Bounds().Dx(), img.Bounds().Dy()
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

// fileHash returns content-based non-cryptographic hash of a file
func fileHash(s string) (uint64, error) {
	f, err := os.Open(s)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	h := fnv.New64a()
	if _, err := io.Copy(h, f); err != nil {
		return 0, err
	}
	return h.Sum64(), nil
}

// imagePhash returns perceptual hash of an image read from the file
func imagePhash(s string) (uint64, error) {
	f, err := os.Open(s)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	img, err := imaging.Decode(f, imaging.AutoOrientation(true))
	if err != nil {
		return 0, err
	}
	return phash.Get(img, func(img image.Image, w, h int) image.Image {
		return imaging.Resize(img, w, h, imaging.Lanczos)
	})
}

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

type galleryCache struct {
	Name     string
	UsePhash bool

	// onceSortPhash guards initial sort of Images by increasing Hash when run
	// with UserPhash=true, so add method can rely on binary search
	onceSortPhash sync.Once

	mu     sync.Mutex
	Images []imageDetails

	// dups is used to track duplicates when UsePhash=false, and
	// imageDetails.Hash holds file-based hash
	dups map[uint64]string // key is imageDetails.Hash, value is imageDetails.Source
	n    int               // number of images added to the gallery during program run
}

// sortByTime sorts gallery dy time in descending order (newest images first)
func (c *galleryCache) sortByTime() {
	sort.Slice(c.Images, func(i, j int) bool {
		return c.Images[i].Time.After(c.Images[j].Time)
	})
}

// minDiff is a phash distance similarity threshold: phash distance above this
// threshold are treated as different images, images with phash distance equal
// or below this threshold are reported as likely duplicates
const minDiff = 5

func (c *galleryCache) addWithPhash(info imageDetails) error {
	c.onceSortPhash.Do(func() {
		sort.SliceStable(c.Images, func(i, j int) bool {
			return c.Images[i].Hash < c.Images[j].Hash
		})
	})
	c.mu.Lock()
	defer c.mu.Unlock()

	i := sort.Search(len(c.Images), func(i int) bool { return c.Images[i].Hash >= info.Hash })

	if i == len(c.Images) {
		if i != 0 {
			info2 := c.Images[i-1]
			if diff := phash.Distance(info.Hash, info2.Hash); diff <= minDiff {
				return fmt.Errorf("possible duplicate (phash similarity distance=%d)"+
					" of %q (source filename %q)", diff, info2.Original, info2.Source)
			}
		}
		c.Images = append(c.Images, info)
		c.n++
		return nil
	}
	if info2 := c.Images[i]; info2.Hash == info.Hash {
		if info2.Source == info.Source && info2.Time.Equal(info.Time) { // attempt to re-add the same image
			return nil
		}
		return fmt.Errorf("duplicate (same phash) of %q (source filename %q)", info2.Original, info2.Source)
	}

	// the index is [i] here, and not [i+1], because this check is *before*
	// info is inserted into c.Images slice, so an element that would be to its
	// right is still at position [i]
	info2 := c.Images[i]
	if diff := phash.Distance(info.Hash, info2.Hash); diff <= minDiff {
		return fmt.Errorf("possible duplicate (phash similarity distance=%d)"+
			" of %q (source filename %q)", diff, info2.Original, info2.Source)
	}
	if i > 0 {
		info2 = c.Images[i-1]
		if diff := phash.Distance(info.Hash, info2.Hash); diff <= minDiff {
			return fmt.Errorf("possible duplicate (phash similarity distance=%d)"+
				" of %q (source filename %q)", diff, info2.Original, info2.Source)
		}
	}

	head := c.Images[:i+1]
	tail := make([]imageDetails, len(c.Images[i:]))
	copy(tail, c.Images[i:])
	head[i] = info
	c.Images = append(head, tail...)
	c.n++
	return nil
}

func (c *galleryCache) add(info imageDetails) error {
	if c.UsePhash {
		return c.addWithPhash(info)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.dups == nil {
		c.dups = make(map[uint64]string, len(c.Images))
		for _, info := range c.Images {
			c.dups[info.Hash] = info.Source
		}
	}
	if s, ok := c.dups[info.Hash]; ok {
		if s == info.Source { // same image, ok to skip
			return nil
		}
		return fmt.Errorf("gallery already has image with id %q: %q (original file name)", info.ID(), s)
	}
	c.Images = append(c.Images, info)
	c.dups[info.Hash] = info.Source
	c.n++
	return nil
}

func loadCache(name string) (*galleryCache, error) {
	f, err := os.Open(name)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	cache := &galleryCache{}
	if err := json.NewDecoder(f).Decode(cache); err != nil {
		return nil, err
	}
	return cache, nil
}

func saveCache(cache *galleryCache, name string) error {
	tf, err := ioutil.TempFile(filepath.Dir(name), "photo-gallery-cache-*.tmp")
	if err != nil {
		return err
	}
	defer tf.Close()
	var defuse bool
	defer func() {
		if !defuse {
			_ = os.Remove(tf.Name())
		}
	}()
	enc := json.NewEncoder(tf)
	enc.SetIndent("", "\t")
	if err := enc.Encode(cache); err != nil {
		return err
	}
	if err := tf.Close(); err != nil {
		return err
	}
	defuse = true
	return os.Rename(tf.Name(), name)
}

var defaultTemplate = template.Must(template.New("gallery").Parse(defaultTemplateBody))

const defaultTemplateBody = `<!DOCTYPE html><head><title>{{.Name}}</title>
<meta charset="utf-8">
<style>
	* {box-sizing: border-box; border: none; font-family: ui-sans-serif, sans-serif;}
	html {background-color: whitesmoke; padding:0;margin:0;}
	body {padding:0;margin:0;}
	header, footer {line-height: 1.7; padding: 5px; background-color: black; color: white;}
	h1 {font-style: bold; font-size:x-large; margin:0;padding:0;}
	footer {text-align: center;}
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
<header><h1>{{.Name}}</h1></header>
<main class="gallery">
{{range .Images}}
	<figure{{if .Portrait}} class="portrait"{{end}}><a href="#{{.ID}}">
	<img loading="lazy" src="{{.Thumbnail}}">
	</a>
	</figure>
{{end}}
</main>
<div class="fullsize-images">
{{range .Images}}
	<figure class="lightbox" id="{{.ID}}">
		<a href="#back">
		<img loading="lazy" src="{{.Original}}">
		</a>
	</figure>
{{end}}
</div>
<footer>&copy; all rights reserved</footer>
</body>
`
