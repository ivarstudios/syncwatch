//go:build ignore

// Export renders syncwatch-icon.svg to the PNGs and the .ico in docs/icon and
// copies the ones the dashboard uses into internal/web/assets. It drives a
// Chromium browser (Edge, Chrome or Chromium) headless:
//
//	go run docs/icon/export.go [-browser PATH]
package main

import (
	"bytes"
	"encoding/binary"
	"flag"
	"fmt"
	"image"
	"image/draw"
	"image/png"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

var (
	sizes    = []int{16, 24, 32, 48, 64, 128, 256, 512, 1024}
	icoSizes = []int{16, 24, 32, 48, 64, 128, 256}
	appFiles = map[string]string{ // docs/icon file -> internal/web/assets file
		"syncwatch-icon.svg":         "syncwatch-icon.svg",
		"syncwatch-icon.ico":         "syncwatch-icon.ico",
		"png/syncwatch-icon-32.png":  "syncwatch-icon-32.png",
		"png/syncwatch-icon-256.png": "syncwatch-icon-256.png",
	}
)

func main() {
	browser := flag.String("browser", "", "Chromium-based browser (default: Edge, Chrome or Chromium)")
	flag.Parse()
	if *browser == "" {
		*browser = findBrowser()
	}
	dir := filepath.Join("docs", "icon")
	svg, err := filepath.Abs(filepath.Join(dir, "syncwatch-icon.svg"))
	check(err)
	tmp, err := os.MkdirTemp("", "syncwatch-icon")
	check(err)
	defer os.RemoveAll(tmp)

	for _, n := range sizes {
		out := filepath.Join(dir, "png", fmt.Sprintf("syncwatch-icon-%d.png", n))
		check(render(*browser, svg, tmp, n, out))
		fmt.Println("wrote", out)
	}
	var icons []string
	for _, n := range icoSizes {
		icons = append(icons, filepath.Join(dir, "png", fmt.Sprintf("syncwatch-icon-%d.png", n)))
	}
	check(writeICO(filepath.Join(dir, "syncwatch-icon.ico"), icons))
	fmt.Println("wrote", filepath.Join(dir, "syncwatch-icon.ico"))
	for from, to := range appFiles {
		b, err := os.ReadFile(filepath.Join(dir, from))
		check(err)
		dst := filepath.Join("internal", "web", "assets", to)
		check(os.WriteFile(dst, b, 0o644))
		fmt.Println("copied", dst)
	}
}

// render draws the SVG n px high, centred on a transparent n x n square.
// Chromium won't open a window smaller than about 200 px, so small sizes are
// drawn top-left on a bigger page and cropped.
func render(browser, svg, tmp string, n int, out string) error {
	win := max(n, 200)
	page := filepath.Join(tmp, "page.html")
	html := fmt.Sprintf(`<!doctype html><body style="margin:0;background:transparent"><div style="width:%dpx;height:%dpx;display:flex;align-items:center;justify-content:center"><img src="%s" style="height:%dpx"></div>`,
		n, n, fileURL(svg), n)
	if err := os.WriteFile(page, []byte(html), 0o644); err != nil {
		return err
	}
	shot := filepath.Join(tmp, "shot.png")
	cmd := exec.Command(browser, "--headless=new", "--disable-gpu", "--hide-scrollbars", "--force-device-scale-factor=1",
		"--default-background-color=00000000", fmt.Sprintf("--window-size=%d,%d", win, win), "--screenshot="+shot, fileURL(page))
	if b, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("%s: %v\n%s", browser, err, b)
	}
	f, err := os.Open(shot)
	if err != nil {
		return err
	}
	src, err := png.Decode(f)
	f.Close()
	if err != nil {
		return err
	}
	dst := image.NewNRGBA(image.Rect(0, 0, n, n))
	draw.Draw(dst, dst.Bounds(), src, image.Point{}, draw.Src)
	w, err := os.Create(out)
	if err != nil {
		return err
	}
	if err := png.Encode(w, dst); err != nil {
		w.Close()
		return err
	}
	return w.Close()
}

// writeICO packs square PNGs (at most 256 px) into one .ico with
// PNG-compressed entries, which Windows reads since Vista.
func writeICO(out string, pngs []string) error {
	var imgs [][]byte
	var px []int
	for _, p := range pngs {
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		cfg, err := png.DecodeConfig(bytes.NewReader(b))
		if err != nil {
			return err
		}
		if cfg.Width != cfg.Height || cfg.Width > 256 {
			return fmt.Errorf("%s is %dx%d; want a square of at most 256 px", p, cfg.Width, cfg.Height)
		}
		imgs, px = append(imgs, b), append(px, cfg.Width)
	}
	var buf bytes.Buffer
	le := func(v any) { _ = binary.Write(&buf, binary.LittleEndian, v) }
	le([3]uint16{0, 1, uint16(len(imgs))}) // reserved, type icon, count
	offset := 6 + 16*len(imgs)
	for i, b := range imgs {
		s := uint8(px[i] % 256) // 0 means 256
		le([4]uint8{s, s, 0, 0})
		le([2]uint16{1, 32}) // planes, bits per pixel
		le([2]uint32{uint32(len(b)), uint32(offset)})
		offset += len(b)
	}
	for _, b := range imgs {
		buf.Write(b)
	}
	return os.WriteFile(out, buf.Bytes(), 0o644)
}

func fileURL(p string) string {
	p = filepath.ToSlash(p)
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	return "file://" + p
}

func findBrowser() string {
	var candidates []string
	switch runtime.GOOS {
	case "windows":
		for _, env := range []string{"ProgramFiles(x86)", "ProgramFiles", "LocalAppData"} {
			if d := os.Getenv(env); d != "" {
				candidates = append(candidates,
					filepath.Join(d, "Microsoft", "Edge", "Application", "msedge.exe"),
					filepath.Join(d, "Google", "Chrome", "Application", "chrome.exe"))
			}
		}
	case "darwin":
		candidates = []string{"/Applications/Google Chrome.app/Contents/MacOS/Google Chrome",
			"/Applications/Microsoft Edge.app/Contents/MacOS/Microsoft Edge", "/Applications/Chromium.app/Contents/MacOS/Chromium"}
	}
	for _, c := range candidates {
		if _, err := os.Stat(c); err == nil {
			return c
		}
	}
	for _, name := range []string{"chromium", "chromium-browser", "google-chrome", "microsoft-edge"} {
		if p, err := exec.LookPath(name); err == nil {
			return p
		}
	}
	log.Fatal("no Chromium-based browser found; pass -browser")
	return ""
}

func check(err error) {
	if err != nil {
		log.Fatal(err)
	}
}
