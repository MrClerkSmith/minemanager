// Package versions talks to the upstream metadata APIs of Vanilla, Paper,
// Purpur, Fabric and Forge to enumerate game versions, builds/loader versions
// and to resolve the exact artifact URL that an instance needs to download.
package versions

import (
	"context"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"mineserver/internal/config"
)

const cacheTTL = 15 * time.Minute

// Build describes one selectable build / loader version of a game version.
type Build struct {
	Build       int       `json:"build,omitempty"`
	Name        string    `json:"name,omitempty"`
	Recommended bool      `json:"recommended,omitempty"`
	DownloadURL string    `json:"download_url,omitempty"`
	Date        time.Time `json:"date,omitempty"`
}

// Artifact is what install() actually needs to fetch.
type Artifact struct {
	// ServerJarURL is downloaded as the runnable server jar (vanilla/paper/purpur).
	ServerJarURL string
	// ServerJarName is the file name to store it under.
	ServerJarName string
	// InstallerURL is a jar that must be executed headlessly (forge).
	InstallerURL string
	// InstallerName is the file name for the installer jar.
	InstallerName string
	// Libraries are downloaded directly, avoiding a JVM installer step
	// (used for Fabric, whose installer requires working JVM TLS).
	Libraries []Library
	// MainClass is the entry point used together with Libraries.
	MainClass string
}

// Library is one runtime dependency stored under libraries/.
type Library struct {
	Name string `json:"name"` // maven coordinate, e.g. net.fabricmc:fabric-loader:0.16.9
	URL  string `json:"url"`  // absolute download url
	Path string `json:"path"` // path relative to the server directory
}

type cacheEntry struct {
	at   time.Time
	data any
}

// API resolves upstream metadata with a short in-memory cache.
type API struct {
	http      *http.Client
	mu        sync.Mutex
	cache     map[string]cacheEntry
	userAgent string
}

// New returns a ready API client. The user agent is sent on every request and
// must identify the manager: PaperMC's downloads service rejects generic ones.
func New(userAgent string) *API {
	if strings.TrimSpace(userAgent) == "" {
		userAgent = "MinecraftServerManager/1.0 (self-hosted manager)"
	}
	return &API{
		http:      &http.Client{Timeout: 30 * time.Second},
		cache:     make(map[string]cacheEntry),
		userAgent: userAgent,
	}
}

func (a *API) cached(key string, fill func() (any, error)) (any, error) {
	a.mu.Lock()
	if e, ok := a.cache[key]; ok && time.Since(e.at) < cacheTTL {
		a.mu.Unlock()
		return e.data, nil
	}
	a.mu.Unlock()
	data, err := fill()
	if err != nil {
		return nil, err
	}
	a.mu.Lock()
	a.cache[key] = cacheEntry{at: time.Now(), data: data}
	a.mu.Unlock()
	return data, nil
}

func (a *API) get(ctx context.Context, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", a.userAgent)
	req.Header.Set("Accept", "application/json")
	resp, err := a.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("GET %s: %w", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("%s: %s: %s", url, resp.Status, strings.TrimSpace(string(body)))
	}
	return io.ReadAll(io.LimitReader(resp.Body, 256<<20))
}

// Versions returns the supported game versions for a server type, newest first.
func (a *API) Versions(ctx context.Context, typ config.ServerType) ([]string, error) {
	var key string
	var fill func() (any, error)
	switch typ {
	case config.TypeVanilla:
		key, fill = "vanilla-versions", func() (any, error) { return a.vanillaVersions(ctx) }
	case config.TypePaper:
		key, fill = "paper-versions", func() (any, error) { return a.paperVersions(ctx) }
	case config.TypePurpur:
		key, fill = "purpur-versions", func() (any, error) { return a.purpurVersions(ctx) }
	case config.TypeFabric:
		key, fill = "fabric-versions", func() (any, error) { return a.fabricVersions(ctx) }
	case config.TypeForge:
		key, fill = "forge-versions", func() (any, error) { return a.forgeVersions(ctx) }
	case config.TypeNeoForge:
		key, fill = "neoforge-versions", func() (any, error) { return a.neoVersions(ctx) }
	default:
		return nil, fmt.Errorf("unsupported type %q", typ)
	}
	v, err := a.cached(key, fill)
	if err != nil {
		return nil, err
	}
	return v.([]string), nil
}

// Builds returns the builds / loader versions available for a game version.
func (a *API) Builds(ctx context.Context, typ config.ServerType, game string) ([]Build, error) {
	switch typ {
	case config.TypePaper:
		return a.paperBuilds(ctx, game)
	case config.TypePurpur:
		return a.purpurBuilds(ctx, game)
	case config.TypeFabric:
		return a.fabricLoaders(ctx, game)
	case config.TypeForge:
		return a.forgeBuilds(ctx, game)
	case config.TypeNeoForge:
		return a.neoBuilds(ctx, game)
	case config.TypeVanilla:
		return []Build{{Name: game, Recommended: true}}, nil
	}
	return nil, fmt.Errorf("unsupported type %q", typ)
}

// Resolve turns a stored ServerConfig into concrete download URLs.
func (a *API) Resolve(ctx context.Context, cfg *config.ServerConfig) (*Artifact, error) {
	switch cfg.Type {
	case config.TypeVanilla:
		return a.resolveVanilla(ctx, cfg.Version)
	case config.TypePaper:
		return a.resolvePaper(ctx, cfg.Version, cfg.Build)
	case config.TypePurpur:
		return a.resolvePurpur(ctx, cfg.Version, cfg.Build)
	case config.TypeFabric:
		return a.resolveFabric(ctx, cfg.Version, cfg.LoaderVersion)
	case config.TypeForge:
		return a.resolveForge(ctx, cfg.Version, cfg.LoaderVersion)
	case config.TypeNeoForge:
		return a.resolveNeoForge(ctx, cfg.Version, cfg.LoaderVersion)
	}
	return nil, fmt.Errorf("unsupported type %q", cfg.Type)
}

// ---------------------------------------------------------------------------
// Vanilla (Mojang piston meta)
// ---------------------------------------------------------------------------

type pistonVersion struct {
	ID   string `json:"id"`
	Type string `json:"type"`
	URL  string `json:"url"`
	Time string `json:"releaseTime"`
}

type pistonManifest struct {
	Versions []pistonVersion `json:"versions"`
}

// sortVersionsDesc sorts Minecraft game versions newest-first. The naive
// string comparison is wrong here ("1.9.4" would outrank "1.21.4"), so the
// numeric release components are compared instead.
func sortVersionsDesc(v []string) {
	sort.SliceStable(v, func(i, j int) bool { return mcVersionLess(v[j], v[i]) })
}

// mcVersionLess reports whether game version a is older than b, e.g.
// 1.8.9 < 1.9.4 < 1.21.4 < 1.21.11 < 26.3.
func mcVersionLess(a, b string) bool {
	pa := strings.Split(strings.SplitN(a, "-", 2)[0], ".")
	pb := strings.Split(strings.SplitN(b, "-", 2)[0], ".")
	for i := 0; i < len(pa) || i < len(pb); i++ {
		var na, nb int
		if i < len(pa) {
			na = leadingInt(pa[i])
		}
		if i < len(pb) {
			nb = leadingInt(pb[i])
		}
		if na != nb {
			return na < nb
		}
	}
	return false
}

// leadingInt parses the leading digits of a version component; anything
// non-numeric (e.g. "RV" in the joke release "1.RV-Pre1") counts as 0.
func leadingInt(s string) int {
	n := 0
	for _, r := range s {
		if r < '0' || r > '9' {
			break
		}
		n = n*10 + int(r-'0')
	}
	return n
}

func (a *API) vanillaVersions(ctx context.Context) ([]string, error) {
	data, err := a.get(ctx, "https://piston-meta.mojang.com/mc/game/version_manifest_v2.json")
	if err != nil {
		return nil, err
	}
	var m pistonManifest
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, err
	}
	out := make([]string, 0, len(m.Versions))
	for _, v := range m.Versions {
		if v.Type == "release" {
			out = append(out, v.ID)
		}
	}
	sortVersionsDesc(out)
	return out, nil
}

func (a *API) resolveVanilla(ctx context.Context, game string) (*Artifact, error) {
	data, err := a.get(ctx, "https://piston-meta.mojang.com/mc/game/version_manifest_v2.json")
	if err != nil {
		return nil, err
	}
	var m pistonManifest
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, err
	}
	for _, v := range m.Versions {
		if v.ID != game {
			continue
		}
		vdata, err := a.get(ctx, v.URL)
		if err != nil {
			return nil, err
		}
		var detail struct {
			Downloads struct {
				Server struct {
					URL string `json:"url"`
				} `json:"server"`
			} `json:"downloads"`
		}
		if err := json.Unmarshal(vdata, &detail); err != nil {
			return nil, err
		}
		if detail.Downloads.Server.URL == "" {
			return nil, fmt.Errorf("version %s has no server jar", game)
		}
		return &Artifact{
			ServerJarURL:  detail.Downloads.Server.URL,
			ServerJarName: fmt.Sprintf("minecraft_server.%s.jar", game),
		}, nil
	}
	return nil, fmt.Errorf("unknown vanilla version %q", game)
}

// ---------------------------------------------------------------------------
// PaperMC
// ---------------------------------------------------------------------------

// fillPaper is PaperMC's downloads service. The api.papermc.io v2 API was
// sunset in 2025 ("410 Gone") and replaced by "fill", which also requires a
// descriptive User-Agent on every request.
const fillPaper = "https://fill.papermc.io/v3/projects/paper"

func (a *API) paperVersions(ctx context.Context) ([]string, error) {
	data, err := a.get(ctx, fillPaper)
	if err != nil {
		return nil, err
	}
	var m struct {
		// versions are grouped by version family, e.g. {"1.21": ["1.21.4", ...]}
		Versions map[string][]string `json:"versions"`
	}
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, err
	}
	out := make([]string, 0, 32)
	for _, vs := range m.Versions {
		for _, v := range vs {
			// Pre-releases and snapshots sort above the real releases and bury
			// them; the dropdown only wants versions people run servers on.
			if !strings.Contains(v, "-") {
				out = append(out, v)
			}
		}
	}
	sortVersionsDesc(out)
	return out, nil
}

func (a *API) paperBuilds(ctx context.Context, game string) ([]Build, error) {
	data, err := a.get(ctx, fillPaper+"/versions/"+url.PathEscape(game)+"/builds")
	if err != nil {
		return nil, err
	}
	recommendedSet := false
	var m []struct {
		ID        int    `json:"id"`
		Channel   string `json:"channel"`
		Time      string `json:"time"`
		Downloads struct {
			Server struct {
				Name string `json:"name"`
				URL  string `json:"url"`
			} `json:"server:default"`
		} `json:"downloads"`
	}
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, err
	}
	out := make([]Build, 0, len(m))
	for _, b := range m {
		t, _ := time.Parse(time.RFC3339, b.Time)
		// The API returns builds newest first, so the first stable one is the
		// recommended build for this game version.
		recommended := !recommendedSet && strings.EqualFold(b.Channel, "stable")
		if recommended {
			recommendedSet = true
		}
		out = append(out, Build{
			Build:       b.ID,
			Name:        fmt.Sprintf("build %d (%s)", b.ID, strings.ToLower(b.Channel)),
			Recommended: recommended,
			Date:        t,
			DownloadURL: b.Downloads.Server.URL,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Build > out[j].Build })
	return out, nil
}

func (a *API) resolvePaper(ctx context.Context, game string, build int) (*Artifact, error) {
	builds, err := a.paperBuilds(ctx, game)
	if err != nil {
		return nil, err
	}
	want := build
	if want <= 0 {
		for _, b := range builds {
			if b.Recommended {
				want = b.Build
				break
			}
		}
	}
	for _, b := range builds {
		if b.Build == want {
			return &Artifact{
				ServerJarURL:  b.DownloadURL,
				ServerJarName: fmt.Sprintf("paper-%s-%d.jar", game, b.Build),
			}, nil
		}
	}
	return nil, fmt.Errorf("no paper build %d for %s", want, game)
}

// ---------------------------------------------------------------------------
// Purpur
// ---------------------------------------------------------------------------

func (a *API) purpurVersions(ctx context.Context) ([]string, error) {
	data, err := a.get(ctx, "https://api.purpurmc.org/v2/purpur")
	if err != nil {
		return nil, err
	}
	var m struct {
		Versions []string `json:"versions"`
	}
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, err
	}
	sortVersionsDesc(m.Versions)
	return m.Versions, nil
}

func (a *API) purpurBuilds(ctx context.Context, game string) ([]Build, error) {
	data, err := a.get(ctx, fmt.Sprintf("https://api.purpurmc.org/v2/purpur/%s", game))
	if err != nil {
		return nil, err
	}
	var m struct {
		Builds struct {
			Latest string          `json:"latest"`
			All    json.RawMessage `json:"all"`
		} `json:"builds"`
	}
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, err
	}

	out := make([]Build, 0)

	// Current API: "all" is a flat array of build numbers (as strings or ints).
	var arr []json.RawMessage
	if err := json.Unmarshal(m.Builds.All, &arr); err == nil {
		for _, item := range arr {
			var s string
			if err := json.Unmarshal(item, &s); err == nil {
				if n, err := strconv.Atoi(s); err == nil {
					out = append(out, Build{
						Build:       n,
						Name:        fmt.Sprintf("build %d", n),
						Recommended: s == m.Builds.Latest,
						DownloadURL: fmt.Sprintf("https://api.purpurmc.org/v2/purpur/%s/%d/download", game, n),
					})
				}
				continue
			}
			var o struct {
				Build int    `json:"build"`
				Date  int64  `json:"date"`
				MD5   string `json:"md5"`
			}
			if err := json.Unmarshal(item, &o); err == nil && o.Build > 0 {
				out = append(out, Build{
					Build:       o.Build,
					Name:        fmt.Sprintf("build %d", o.Build),
					Recommended: strconv.Itoa(o.Build) == m.Builds.Latest,
					Date:        time.Unix(o.Date, 0),
					DownloadURL: fmt.Sprintf("https://api.purpurmc.org/v2/purpur/%s/%d/download", game, o.Build),
				})
			}
		}
	} else {
		// Legacy API: "all" is a map of build id to metadata.
		var legacy map[string]struct {
			Build  int    `json:"build"`
			Date   int64  `json:"date"`
			Commit string `json:"commit"`
		}
		if err := json.Unmarshal(m.Builds.All, &legacy); err != nil {
			return nil, err
		}
		for _, b := range legacy {
			out = append(out, Build{
				Build:       b.Build,
				Name:        fmt.Sprintf("build %d", b.Build),
				Recommended: fmt.Sprint(b.Build) == m.Builds.Latest,
				Date:        time.Unix(b.Date, 0),
				DownloadURL: fmt.Sprintf("https://api.purpurmc.org/v2/purpur/%s/%d/download", game, b.Build),
			})
		}
	}

	sort.Slice(out, func(i, j int) bool { return out[i].Build > out[j].Build })
	return out, nil
}

func (a *API) resolvePurpur(ctx context.Context, game string, build int) (*Artifact, error) {
	builds, err := a.purpurBuilds(ctx, game)
	if err != nil {
		return nil, err
	}
	want := build
	if want <= 0 {
		for _, b := range builds {
			if b.Recommended {
				want = b.Build
				break
			}
		}
	}
	for _, b := range builds {
		if b.Build == want {
			return &Artifact{
				ServerJarURL:  b.DownloadURL,
				ServerJarName: fmt.Sprintf("purpur-%s-%d.jar", game, b.Build),
			}, nil
		}
	}
	return nil, fmt.Errorf("no purpur build %d for %s", want, game)
}

// ---------------------------------------------------------------------------
// Fabric
// ---------------------------------------------------------------------------

func (a *API) fabricVersions(ctx context.Context) ([]string, error) {
	data, err := a.get(ctx, "https://meta.fabricmc.net/v2/versions/game")
	if err != nil {
		return nil, err
	}
	var m []struct {
		Version string `json:"version"`
		Stable  bool   `json:"stable"`
	}
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, err
	}
	out := make([]string, 0, len(m))
	for _, v := range m {
		// Snapshots, combat tests and pre-releases bury the actual releases;
		// the dropdown only wants the versions people run servers on.
		if v.Stable {
			out = append(out, v.Version)
		}
	}
	sortVersionsDesc(out)
	return out, nil
}

func (a *API) fabricLoaders(ctx context.Context, game string) ([]Build, error) {
	data, err := a.get(ctx, fmt.Sprintf("https://meta.fabricmc.net/v2/versions/loader/%s", game))
	if err != nil {
		return nil, err
	}
	var m []struct {
		Loader struct {
			Version string `json:"version"`
			Stable  bool   `json:"stable"`
		} `json:"loader"`
	}
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, err
	}
	out := make([]Build, 0, len(m))
	for _, l := range m {
		out = append(out, Build{
			Name: l.Loader.Version,
			DownloadURL: fmt.Sprintf("https://meta.fabricmc.net/v2/versions/loader/%s/%s",
				game, l.Loader.Version),
		})
	}
	// Newest loader first, ignoring the endpoint's own ordering.
	sort.SliceStable(out, func(i, j int) bool {
		return mcVersionLess(out[j].Name, out[i].Name)
	})
	if len(out) > 0 {
		out[0].Recommended = true
	}
	return out, nil
}

func (a *API) resolveFabric(ctx context.Context, game, loader string) (*Artifact, error) {
	if loader == "" {
		builds, err := a.fabricLoaders(ctx, game)
		if err != nil {
			return nil, err
		}
		if len(builds) == 0 {
			return nil, fmt.Errorf("no fabric loader for %s", game)
		}
		loader = builds[0].Name
	}

	// The game jar comes from Mojang; fabric only adds loader + libraries.
	vanilla, err := a.resolveVanilla(ctx, game)
	if err != nil {
		return nil, err
	}

	// The loader meta lists every runtime library and the server main class,
	// so the whole server can be assembled without ever running the fabric
	// installer jar (which needs working JVM TLS on the host).
	data, err := a.get(ctx,
		fmt.Sprintf("https://meta.fabricmc.net/v2/versions/loader/%s/%s", game, loader))
	if err != nil {
		return nil, err
	}
	var meta struct {
		Loader struct {
			Maven string `json:"maven"`
		} `json:"loader"`
		Intermediary struct {
			Maven string `json:"maven"`
		} `json:"intermediary"`
		LauncherMeta struct {
			Libraries struct {
				Common []struct {
					Name string `json:"name"`
					URL  string `json:"url"`
				} `json:"common"`
			} `json:"libraries"`
			MainClass struct {
				Server string `json:"server"`
			} `json:"mainClass"`
		} `json:"launcherMeta"`
	}
	if err := json.Unmarshal(data, &meta); err != nil {
		return nil, err
	}
	if meta.LauncherMeta.MainClass.Server == "" {
		return nil, fmt.Errorf("fabric loader meta for %s has no server main class", game)
	}

	libs := make([]Library, 0, len(meta.LauncherMeta.Libraries.Common)+2)
	for _, l := range meta.LauncherMeta.Libraries.Common {
		libs = append(libs, mavenLibrary(l.Name, l.URL))
	}
	libs = append(libs, mavenLibrary(meta.Loader.Maven, "https://maven.fabricmc.net/"))
	libs = append(libs, mavenLibrary(meta.Intermediary.Maven, "https://maven.fabricmc.net/"))

	return &Artifact{
		ServerJarURL:  vanilla.ServerJarURL,
		ServerJarName: vanilla.ServerJarName,
		Libraries:     libs,
		MainClass:     meta.LauncherMeta.MainClass.Server,
	}, nil
}

// mavenLibrary turns a "group:name:version" coordinate plus a repository url
// into a concrete download location inside the server's libraries/ tree.
func mavenLibrary(coord, repo string) Library {
	parts := strings.SplitN(coord, ":", 3)
	if len(parts) != 3 {
		return Library{Name: coord}
	}
	group, name, version := parts[0], parts[1], parts[2]
	groupPath := strings.ReplaceAll(group, ".", "/")
	fileName := fmt.Sprintf("%s-%s.jar", name, version)
	return Library{
		Name: coord,
		URL:  strings.TrimSuffix(repo, "/") + "/" + groupPath + "/" + name + "/" + version + "/" + fileName,
		Path: "libraries/" + groupPath + "/" + name + "/" + version + "/" + fileName,
	}
}

// ---------------------------------------------------------------------------
// Forge
// ---------------------------------------------------------------------------

type mavenMetadata struct {
	XMLName    xml.Name `xml:"metadata"`
	Versioning struct {
		Release  string `xml:"release"`
		Versions struct {
			Version []string `xml:"version"`
		} `xml:"versions"`
		LastUpdated string `xml:"lastUpdated"`
	} `xml:"versioning"`
}

// forgeMaven is Forge's maven repository. The old files.minecraftforge.net
// host stopped serving metadata in 2025.
const forgeMaven = "https://maven.minecraftforge.net/net/minecraftforge/forge"

// mavenVersions returns the artifact versions advertised by a maven repository.
func (a *API) mavenVersions(ctx context.Context, repo string) ([]string, error) {
	data, err := a.get(ctx, repo+"/maven-metadata.xml")
	if err != nil {
		return nil, err
	}
	var meta mavenMetadata
	if err := xml.Unmarshal(data, &meta); err != nil {
		return nil, err
	}
	return meta.Versioning.Versions.Version, nil
}

func (a *API) forgeVersions(ctx context.Context) ([]string, error) {
	data, err := a.get(ctx, forgeMaven+"/maven-metadata.xml")
	if err != nil {
		return nil, err
	}
	var meta mavenMetadata
	if err := xml.Unmarshal(data, &meta); err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	out := make([]string, 0, len(meta.Versioning.Versions.Version))
	// metadata is oldest-first; iterate newest-first so the first entry per
	// game version is its latest build.
	for i := len(meta.Versioning.Versions.Version) - 1; i >= 0; i-- {
		v := meta.Versioning.Versions.Version[i]
		game := v
		if idx := strings.Index(v, "-"); idx > 0 {
			game = v[:idx]
		}
		if seen[game] {
			continue
		}
		seen[game] = true
		out = append(out, game)
	}
	sortVersionsDesc(out)
	return out, nil
}

func (a *API) forgeBuilds(ctx context.Context, game string) ([]Build, error) {
	data, err := a.get(ctx, forgeMaven+"/maven-metadata.xml")
	if err != nil {
		return nil, err
	}
	var meta mavenMetadata
	if err := xml.Unmarshal(data, &meta); err != nil {
		return nil, err
	}
	out := make([]Build, 0)
	for i := len(meta.Versioning.Versions.Version) - 1; i >= 0; i-- {
		v := meta.Versioning.Versions.Version[i]
		if !strings.HasPrefix(v, game+"-") {
			continue
		}
		out = append(out, Build{
			Name:        v,
			Recommended: v == meta.Versioning.Release,
			DownloadURL: fmt.Sprintf("%s/%s/forge-%s-installer.jar", forgeMaven, v, v),
		})
	}
	return out, nil
}

func (a *API) resolveForge(ctx context.Context, game, full string) (*Artifact, error) {
	if full == "" || !strings.HasPrefix(full, game+"-") {
		builds, err := a.forgeBuilds(ctx, game)
		if err != nil {
			return nil, err
		}
		if len(builds) == 0 {
			return nil, fmt.Errorf("no forge build for %s", game)
		}
		for _, b := range builds {
			if b.Recommended {
				full = b.Name
				break
			}
		}
		if full == "" {
			full = builds[0].Name
		}
	}
	return &Artifact{
		InstallerURL:  fmt.Sprintf("%s/%s/forge-%s-installer.jar", forgeMaven, full, full),
		InstallerName: fmt.Sprintf("forge-%s-installer.jar", full),
		ServerJarName: fmt.Sprintf("forge-%s-server.jar", full),
	}, nil
}

// ---------------------------------------------------------------------------
// NeoForge
// ---------------------------------------------------------------------------

// neoMaven is NeoForge's artifact repository. Its version numbers embed the
// Minecraft version without the leading "1.", e.g. 21.1.251 -> Minecraft 1.21.1.
const neoMaven = "https://maven.neoforged.net/releases/net/neoforged/neoforge"

// neoVersions lists the Minecraft versions that have a NeoForge build, newest
// first. The mapping from a NeoForge build to its game version is derived by
// matching the build's "major.minor" prefix against the known vanilla versions.
func (a *API) neoVersions(ctx context.Context) ([]string, error) {
	versions, err := a.mavenVersions(ctx, neoMaven)
	if err != nil {
		return nil, err
	}
	known, err := a.Versions(ctx, config.TypeVanilla)
	if err != nil {
		return nil, err
	}
	have := make(map[string]bool, len(versions))
	for _, v := range versions {
		have[neoMCPrefix(v)] = true
	}
	out := make([]string, 0, len(known))
	for _, mc := range known { // known is newest-first already
		if have[strings.TrimPrefix(mc, "1.")] {
			out = append(out, mc)
		}
	}
	return out, nil
}

// neoMCPrefix returns the "major.minor" part of a NeoForge build number, which
// is the Minecraft version with the leading "1." dropped.
func neoMCPrefix(build string) string {
	parts := strings.SplitN(build, ".", 3)
	if len(parts) < 2 {
		return build
	}
	return parts[0] + "." + parts[1]
}

// neoBuilds lists the NeoForge builds for a Minecraft version, newest first.
func (a *API) neoBuilds(ctx context.Context, game string) ([]Build, error) {
	versions, err := a.mavenVersions(ctx, neoMaven)
	if err != nil {
		return nil, err
	}
	prefix := strings.TrimPrefix(game, "1.") + "."
	out := make([]Build, 0, 16)
	for _, v := range versions {
		if !strings.HasPrefix(v, prefix) {
			continue
		}
		out = append(out, Build{
			Name:        v,
			Recommended: !neoIsPrerelease(v),
			DownloadURL: fmt.Sprintf("%s/%s/neoforge-%s-installer.jar", neoMaven, v, v),
		})
	}
	sort.Slice(out, func(i, j int) bool { return neoNewer(out[i].Name, out[j].Name) })
	// Only the newest release build should be the recommended one.
	recommended := false
	for i := range out {
		if !recommended && out[i].Recommended {
			recommended = true
		} else {
			out[i].Recommended = false
		}
	}
	return out, nil
}

// neoIsPrerelease reports whether a build tag is a beta or alpha rather than a
// proper release.
func neoIsPrerelease(build string) bool {
	return strings.Contains(build, "-beta") || strings.Contains(build, "-alpha")
}

// neoNewer reports whether build a is newer than build b, comparing the numeric
// components from the most significant to the least, with pre-release markers
// sorting below final releases.
func neoNewer(a, b string) bool {
	pa := strings.SplitN(strings.SplitN(a, "-", 2)[0], ".", 4)
	pb := strings.SplitN(strings.SplitN(b, "-", 2)[0], ".", 4)
	for i := 0; i < 4; i++ {
		var na, nb int
		if i < len(pa) {
			na, _ = strconv.Atoi(pa[i])
		}
		if i < len(pb) {
			nb, _ = strconv.Atoi(pb[i])
		}
		if na != nb {
			return na > nb
		}
	}
	// Same base version: a final release (no marker) is newer than a beta.
	return neoIsPrerelease(b) && !neoIsPrerelease(a)
}

func (a *API) resolveNeoForge(ctx context.Context, game, full string) (*Artifact, error) {
	builds, err := a.neoBuilds(ctx, game)
	if err != nil {
		return nil, err
	}
	if full == "" {
		if len(builds) == 0 {
			return nil, fmt.Errorf("no neoforge build for %s", game)
		}
		for _, b := range builds {
			if b.Recommended {
				full = b.Name
				break
			}
		}
		if full == "" {
			full = builds[0].Name
		}
	}
	return &Artifact{
		InstallerURL:  fmt.Sprintf("%s/%s/neoforge-%s-installer.jar", neoMaven, full, full),
		InstallerName: fmt.Sprintf("neoforge-%s-installer.jar", full),
		ServerJarName: fmt.Sprintf("neoforge-%s-server.jar", full),
	}, nil
}
