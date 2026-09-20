// Command genblocks renders pixel-art Minecraft-style block textures as
// 16x16 PNGs for the server icon picker. The assets are generated locally
// instead of shipping copyrighted game textures.
package main

import (
	"image"
	"image/color"
	"image/png"
	"math/rand"
	"os"
	"path/filepath"
)

const size = 16

// palette returns the colors of one block type as a flat 16x16 grid.
type block struct {
	name   string
	pixels func(r *rand.Rand) [size][size]color.RGBA
}

func rgb(r, g, b uint8) color.RGBA { return color.RGBA{r, g, b, 255} }

// jitter slightly randomizes a color's brightness, the classic Minecraft
// texture noise.
func jitter(r *rand.Rand, c color.RGBA, amount int) color.RGBA {
	d := int(r.Intn(2*amount+1) - amount)
	clamp := func(v int) uint8 {
		if v < 0 {
			return 0
		}
		if v > 255 {
			return 255
		}
		return uint8(v)
	}
	return color.RGBA{clamp(int(c.R) + d), clamp(int(c.G) + d), clamp(int(c.B) + d), 255}
}

// noisy fills the whole grid with one color plus brightness jitter.
func noisy(base color.RGBA) func(r *rand.Rand) [size][size]color.RGBA {
	return func(r *rand.Rand) [size][size]color.RGBA {
		var out [size][size]color.RGBA
		for y := range out {
			for x := range out[y] {
				out[y][x] = jitter(r, base, 12)
			}
		}
		return out
	}
}

// speckle scatters blobs of a color over a noisy base.
func speckle(base, spot color.RGBA, blobs, radius int) func(r *rand.Rand) [size][size]color.RGBA {
	return func(r *rand.Rand) [size][size]color.RGBA {
		out := noisy(base)(r)
		for i := 0; i < blobs; i++ {
			cx, cy := r.Intn(size), r.Intn(size)
			for dy := -radius; dy <= radius; dy++ {
				for dx := -radius; dx <= radius; dx++ {
					if dx*dx+dy*dy > radius*radius {
						continue
					}
					x, y := cx+dx, cy+dy
					if x < 0 || y < 0 || x >= size || y >= size {
						continue
					}
					out[y][x] = jitter(r, spot, 14)
				}
			}
		}
		return out
	}
}

// planks draws wood planks: horizontal boards separated by darker seams.
func planks(base color.RGBA) func(r *rand.Rand) [size][size]color.RGBA {
	return func(r *rand.Rand) [size][size]color.RGBA {
		out := noisy(base)(r)
		seam := shade(base, -45)
		for y := 0; y < size; y += 4 {
			for x := 0; x < size; x++ {
				out[y][x] = jitter(r, seam, 8)
			}
		}
		return out
	}
}

// wicker draws a hay bale: dense horizontal lines.
func wicker(base color.RGBA) func(r *rand.Rand) [size][size]color.RGBA {
	return func(r *rand.Rand) [size][size]color.RGBA {
		out := noisy(base)(r)
		line := shade(base, -40)
		for y := 0; y < size; y += 2 {
			for x := 0; x < size; x++ {
				out[y][x] = jitter(r, line, 8)
			}
		}
		return out
	}
}

// brick draws a brick wall with mortar lines.
func brick(base, mortar color.RGBA) func(r *rand.Rand) [size][size]color.RGBA {
	return func(r *rand.Rand) [size][size]color.RGBA {
		out := noisy(base)(r)
		for y := 0; y < size; y++ {
			if y%4 == 3 {
				for x := 0; x < size; x++ {
					out[y][x] = mortar
				}
			} else {
				offset := (y / 4) % 2 * 4
				for x := 0; x < size; x++ {
					if (x+offset)%8 == 7 {
						out[y][x] = mortar
					}
				}
			}
		}
		return out
	}
}

// stripes draws vertical stripes (melon / pumpkin / TNT banding).
func stripes(base, stripe color.RGBA, gap int) func(r *rand.Rand) [size][size]color.RGBA {
	return func(r *rand.Rand) [size][size]color.RGBA {
		out := noisy(base)(r)
		for x := 0; x < size; x++ {
			if x%gap == 0 {
				for y := 0; y < size; y++ {
					out[y][x] = jitter(r, stripe, 8)
				}
			}
		}
		return out
	}
}

// grassBlock: green top with soil below and a ragged boundary.
func grassBlock(r *rand.Rand) [size][size]color.RGBA {
	soil := rgb(122, 85, 53)
	grass := rgb(96, 160, 74)
	dark := rgb(74, 124, 58)
	out := noisy(soil)(r)
	for x := 0; x < size; x++ {
		h := 3 + r.Intn(3)
		for y := 0; y < h; y++ {
			out[y][x] = jitter(r, grass, 12)
			if y == h-1 && r.Intn(2) == 0 {
				out[y][x] = jitter(r, dark, 10)
			}
		}
	}
	return out
}

// oreBlock: stone base with gem blobs.
func oreBlock(gem color.RGBA) func(r *rand.Rand) [size][size]color.RGBA {
	return speckle(rgb(128, 128, 130), gem, 6, 2)
}

// shade darkens or lightens a color.
func shade(c color.RGBA, delta int) color.RGBA {
	clamp := func(v int) uint8 {
		if v < 0 {
			return 0
		}
		if v > 255 {
			return 255
		}
		return uint8(v)
	}
	return color.RGBA{clamp(int(c.R) + delta), clamp(int(c.G) + delta), clamp(int(c.B) + delta), 255}
}

var blocks = []block{
	{"grass", grassBlock},
	{"dirt", noisy(rgb(122, 85, 53))},
	{"stone", noisy(rgb(128, 128, 130))},
	{"cobblestone", speckle(rgb(125, 125, 127), rgb(95, 95, 97), 9, 2)},
	{"sand", noisy(rgb(219, 203, 154))},
	{"red_sand", noisy(rgb(191, 124, 72))},
	{"gravel", speckle(rgb(130, 125, 120), rgb(100, 95, 90), 10, 2)},
	{"oak_planks", planks(rgb(160, 131, 84))},
	{"spruce_planks", planks(rgb(101, 75, 45))},
	{"oak_log", stripes(rgb(160, 131, 84), rgb(101, 75, 45), 4)},
	{"brick", brick(rgb(150, 97, 80), rgb(214, 200, 180))},
	{"coal_ore", oreBlock(rgb(30, 30, 32))},
	{"iron_ore", oreBlock(rgb(201, 169, 143))},
	{"gold_ore", oreBlock(rgb(222, 185, 55))},
	{"diamond_ore", oreBlock(rgb(108, 219, 226))},
	{"emerald_ore", oreBlock(rgb(40, 176, 96))},
	{"lapis_ore", oreBlock(rgb(50, 90, 196))},
	{"redstone_ore", oreBlock(rgb(191, 45, 45))},
	{"quartz_ore", oreBlock(rgb(240, 240, 240))},
	{"coal_block", noisy(rgb(28, 28, 30))},
	{"iron_block", noisy(rgb(224, 224, 226))},
	{"gold_block", noisy(rgb(229, 190, 49))},
	{"diamond_block", noisy(rgb(108, 219, 226))},
	{"emerald_block", noisy(rgb(40, 176, 96))},
	{"lapis_block", noisy(rgb(50, 90, 196))},
	{"redstone_block", noisy(rgb(191, 45, 45))},
	{"quartz_block", noisy(rgb(235, 235, 238))},
	{"obsidian", speckle(rgb(30, 26, 42), rgb(90, 60, 140), 5, 1)},
	{"netherrack", speckle(rgb(110, 45, 45), rgb(75, 30, 30), 10, 2)},
	{"end_stone", noisy(rgb(223, 218, 158))},
	{"glowstone", speckle(rgb(222, 174, 70), rgb(255, 236, 140), 8, 1)},
	{"pumpkin", stripes(rgb(218, 120, 43), rgb(165, 85, 25), 3)},
	{"melon", stripes(rgb(140, 196, 90), rgb(95, 145, 60), 3)},
	{"hay", wicker(rgb(217, 191, 92))},
	{"sponge", speckle(rgb(218, 194, 84), rgb(150, 125, 50), 9, 1)},
	{"tnt", func(r *rand.Rand) [size][size]color.RGBA {
		out := stripes(rgb(191, 45, 45), rgb(230, 230, 230), 5)(r)
		for y := range out { // dark band for the TNT label strip
			for x := 5; x < 8; x++ {
				out[y][x] = jitter(r, rgb(120, 25, 25), 8)
			}
		}
		return out
	}},
	{"snow", noisy(rgb(240, 244, 248))},
	{"ice", noisy(rgb(158, 205, 233))},
	{"water", noisy(rgb(64, 110, 190))},
	{"lava", speckle(rgb(224, 92, 22), rgb(255, 196, 64), 8, 1)},
}

func main() {
	out := "internal/web/static/blocks"
	if err := os.MkdirAll(out, 0o755); err != nil {
		panic(err)
	}
	r := rand.New(rand.NewSource(42))
	for _, b := range blocks {
		img := image.NewRGBA(image.Rect(0, 0, size, size))
		px := b.pixels(r)
		for y := 0; y < size; y++ {
			for x := 0; x < size; x++ {
				img.Set(x, y, px[y][x])
			}
		}
		path := filepath.Join(out, b.name+".png")
		f, err := os.Create(path)
		if err != nil {
			panic(err)
		}
		if err := png.Encode(f, img); err != nil {
			f.Close()
			panic(err)
		}
		f.Close()
	}
	println("wrote", len(blocks), "blocks to", out)
}
