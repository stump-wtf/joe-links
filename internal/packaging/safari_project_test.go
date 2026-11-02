// safari_project_test.go guards the SPEC-0015 Xcode project invariants that are
// checkable without a Mac: every path the committed project references must
// exist, every file the manifest loads must also be a resource of both Safari
// extension targets, and the project must keep both platform targets at their
// documented deployment floors.
//
// The unsigned xcode-build job proves the project compiles, but it cannot
// notice a file the project was never told to copy: a new extension source
// that skips the Xcode step builds green and then 404s inside Safari. That
// silent path is what these guards close, on Linux, on every pull request.
// Issue #133, SPEC-0015 REQ "Xcode Project Generation" and
// "Xcode Project Maintenance".
package packaging_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
)

const (
	appleProjectDir = "integrations/apple"
	pbxprojPath     = "integrations/apple/joe-links.xcodeproj/project.pbxproj"
	extensionDir    = "integrations/extension"
)

// pbx is the slice of project.pbxproj these guards need: top-level objects by
// ID, read straight from the old-style plist rather than a plist parser, so the
// check has no dependency Xcode or macOS tooling would have to supply.
type pbx struct {
	objects map[string]string
}

var pbxObjectStart = regexp.MustCompile(`(?m)^\t\t([0-9A-F]{24})(?: /\* .*? \*/)? = \{`)

func readPBX(t *testing.T) *pbx {
	t.Helper()
	src := readRepoFile(t, pbxprojPath)
	p := &pbx{objects: map[string]string{}}
	for _, m := range pbxObjectStart.FindAllStringSubmatchIndex(src, -1) {
		id := src[m[2]:m[3]]
		open := strings.Index(src[m[0]:], "{") + m[0]
		depth := 0
		for i := open; i < len(src); i++ {
			switch src[i] {
			case '{':
				depth++
			case '}':
				depth--
				if depth == 0 {
					p.objects[id] = src[open+1 : i]
					i = len(src)
				}
			}
		}
	}
	if len(p.objects) == 0 {
		t.Fatalf("%s: no objects parsed, the guards below would pass vacuously", pbxprojPath)
	}
	return p
}

func (p *pbx) isa(id string) string { return p.value(id, "isa") }

// value returns the scalar assigned to key by one object. File and build-file
// objects are written on a single line and the rest one key per line, so the
// assignment is located by scanning rather than by line shape, and the
// shallowest assignment wins so a nested object cannot answer for its parent.
func (p *pbx) value(id, key string) string {
	body := p.objects[id]
	want := key + " = "
	best, bestDepth := "", 1<<30
	for i := 0; ; {
		at := strings.Index(body[i:], want)
		if at < 0 {
			break
		}
		at += i
		if at == 0 || !isWordByte(body[at-1]) {
			depth := strings.Count(body[:at], "{") + strings.Count(body[:at], "(") -
				strings.Count(body[:at], "}") - strings.Count(body[:at], ")")
			if depth < bestDepth {
				best, bestDepth = scalarValue(body[at+len(want):]), depth
			}
		}
		i = at + len(want)
	}
	return strings.Trim(best, `"`)
}

// scalarValue returns the assignment value up to its terminating semicolon,
// ignoring semicolons nested inside braces or lists.
func scalarValue(value string) string {
	depth := 0
	for i := 0; i < len(value); i++ {
		switch value[i] {
		case '{', '(':
			depth++
		case '}', ')':
			depth--
		case ';':
			if depth == 0 {
				return trimPBXComment(value[:i])
			}
		}
	}
	return trimPBXComment(value)
}

// trimPBXComment drops the /* comment */ Xcode writes beside an ID, which is
// annotation rather than part of the value.
func trimPBXComment(value string) string {
	return strings.TrimSpace(pbxComment.ReplaceAllString(value, ""))
}

var pbxComment = regexp.MustCompile(`/\*[^*]*\*+(?:[^/*][^*]*\*+)*/`)

func isWordByte(b byte) bool {
	return b == '_' || (b >= '0' && b <= '9') || (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z')
}

var pbxID = regexp.MustCompile(`[0-9A-F]{24}`)

// list returns the object IDs inside a `key = ( ... );` block of one object.
func (p *pbx) list(id, key string) []string {
	body := p.objects[id]
	at := strings.Index(body, key+" = (")
	if at < 0 {
		return nil
	}
	rest := body[at:]
	depth := 0
	for i := strings.Index(rest, "("); i < len(rest); i++ {
		switch rest[i] {
		case '(':
			depth++
		case ')':
			if depth--; depth == 0 {
				var ids []string
				seen := map[string]bool{}
				for _, found := range pbxID.FindAllString(rest[:i], -1) {
					if !seen[found] {
						seen[found] = true
						ids = append(ids, found)
					}
				}
				return ids
			}
		}
	}
	return nil
}

func (p *pbx) withISA(want string) []string {
	var ids []string
	for id := range p.objects {
		if p.isa(id) == want {
			ids = append(ids, id)
		}
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	return ids
}

// pathsInGroups resolves every file reference a group can reach, relative to
// the repository root. A reference is relative to its group's path, except
// where sourceTree says SOURCE_ROOT, which anchors it at the project directory
// instead: the extension sources hang off a SOURCE_ROOT group as
// ../../extension/<file>, which lands in integrations/extension/. Built
// products (sourceTree BUILT_PRODUCTS_DIR) are products of a build, not files
// in the tree, so they are skipped.
func (p *pbx) pathsInGroups() map[string]string {
	resolved := map[string]string{}
	anchor := func(id, prefix string) string {
		if p.value(id, "sourceTree") == "SOURCE_ROOT" {
			return appleProjectDir
		}
		return prefix
	}
	nameOf := func(id string) string {
		if name := p.value(id, "path"); name != "" {
			return name
		}
		return p.value(id, "name")
	}
	var walk func(groupID, prefix string)
	walk = func(groupID, prefix string) {
		for _, child := range p.list(groupID, "children") {
			switch p.isa(child) {
			case "PBXGroup":
				walk(child, filepath.Join(anchor(child, prefix), nameOf(child)))
			case "PBXFileReference":
				switch p.value(child, "sourceTree") {
				case "<group>", "SOURCE_ROOT":
					resolved[child] = filepath.Join(anchor(child, prefix), nameOf(child))
				}
			}
		}
	}
	for _, project := range p.withISA("PBXProject") {
		if main := p.value(project, "mainGroup"); main != "" {
			walk(main, appleProjectDir)
		}
	}
	return resolved
}

// extensionTargetResources returns, per Safari extension target name, the
// repository-relative paths that target copies into the extension bundle.
func (p *pbx) extensionTargetResources(t *testing.T) map[string][]string {
	t.Helper()
	resolved := p.pathsInGroups()
	byTarget := map[string][]string{}
	for _, target := range p.withISA("PBXNativeTarget") {
		if !strings.Contains(p.value(target, "productType"), "app-extension") {
			continue
		}
		var paths []string
		for _, phase := range p.list(target, "buildPhases") {
			if p.isa(phase) != "PBXResourcesBuildPhase" {
				continue
			}
			for _, buildFile := range p.list(phase, "files") {
				ref := p.value(buildFile, "fileRef")
				path, ok := resolved[ref]
				if !ok {
					t.Errorf("%s: resources build file %s references %s, which no group reaches",
						p.value(target, "name"), buildFile, ref)
					continue
				}
				paths = append(paths, path)
			}
		}
		byTarget[p.value(target, "name")] = paths
	}
	return byTarget
}

// includesFile reports whether a resource list carries file, directly or
// through a directory reference such as the icons/ folder.
func includesFile(paths []string, file string) bool {
	for _, path := range paths {
		if path == file {
			return true
		}
		if info, err := os.Stat(filepath.Join("..", "..", path)); err == nil && info.IsDir() &&
			strings.HasPrefix(file, path+"/") {
			return true
		}
	}
	return false
}

func missingFrom(byTarget map[string][]string, file string) []string {
	var targets []string
	for name, paths := range byTarget {
		if !includesFile(paths, file) {
			targets = append(targets, name)
		}
	}
	sort.Strings(targets)
	return targets
}

func TestXcodeProjectReferencesExistOnDisk(t *testing.T) {
	p := readPBX(t)
	resolved := p.pathsInGroups()
	if len(resolved) == 0 {
		t.Fatalf("%s: no group file references resolved, the guard would pass vacuously", pbxprojPath)
	}
	for ref, path := range resolved {
		if _, err := os.Stat(filepath.Join("..", "..", path)); err != nil {
			t.Errorf("%s: file reference %s points at %s, which does not exist (%v); "+
				"the Safari build would ship without it", pbxprojPath, ref, path, err)
		}
	}
}

func TestManifestFilesAreResourcesOfBothExtensionTargets(t *testing.T) {
	p := readPBX(t)
	byTarget := p.extensionTargetResources(t)
	if len(byTarget) < 2 {
		t.Fatalf("expected an iOS and a macOS Safari extension target, found %v", keysOf(byTarget))
	}

	raw := readRepoFile(t, filepath.Join(extensionDir, "manifest.json"))
	var manifest struct {
		Background struct {
			ServiceWorker string   `json:"service_worker"`
			Scripts       []string `json:"scripts"`
		} `json:"background"`
		ContentScripts []struct {
			JS  []string `json:"js"`
			CSS []string `json:"css"`
		} `json:"content_scripts"`
		Icons  map[string]string `json:"icons"`
		Action struct {
			DefaultPopup string            `json:"default_popup"`
			DefaultIcon  map[string]string `json:"default_icon"`
		} `json:"action"`
		OptionsUI struct {
			Page string `json:"page"`
		} `json:"options_ui"`
	}
	if err := json.Unmarshal([]byte(raw), &manifest); err != nil {
		t.Fatalf("parsing %s/manifest.json: %v", extensionDir, err)
	}

	referenced := map[string]bool{}
	add := func(ref string) {
		if ref == "" || strings.Contains(ref, "://") || strings.HasPrefix(ref, "data:") {
			return
		}
		referenced[filepath.Clean(filepath.FromSlash(ref))] = true
	}
	add(manifest.Background.ServiceWorker)
	for _, script := range manifest.Background.Scripts {
		add(script)
	}
	add(manifest.Action.DefaultPopup)
	add(manifest.OptionsUI.Page)
	for _, icon := range manifest.Icons {
		add(icon)
	}
	for _, icon := range manifest.Action.DefaultIcon {
		add(icon)
	}
	for _, group := range manifest.ContentScripts {
		for _, script := range group.JS {
			add(script)
		}
		for _, style := range group.CSS {
			add(style)
		}
	}
	if len(referenced) == 0 {
		t.Fatalf("%s/manifest.json references no files, the guard would pass vacuously", extensionDir)
	}

	for _, file := range sortedKeys(referenced) {
		if _, err := os.Stat(filepath.Join("..", "..", extensionDir, file)); err != nil {
			t.Errorf("manifest.json references %s, which does not exist (%v)", file, err)
			continue
		}
		if missing := missingFrom(byTarget, filepath.Join(extensionDir, file)); len(missing) > 0 {
			t.Errorf("%s is loaded by manifest.json but is not a resource of %v: the Safari build "+
				"would omit it while the unpacked extension works, so add it to the Resources phase of "+
				"both extension targets in %s", file, missing, pbxprojPath)
		}
	}
}

// TestExtensionSourcesAreResourcesOfBothExtensionTargets covers the files a
// manifest cannot name on its own, such as the script a popup page loads.
func TestExtensionSourcesAreResourcesOfBothExtensionTargets(t *testing.T) {
	p := readPBX(t)
	byTarget := p.extensionTargetResources(t)

	var sources []string
	err := filepath.WalkDir(filepath.Join("..", "..", extensionDir), func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		name := entry.Name()
		if strings.HasSuffix(name, ".test.mjs") || strings.HasSuffix(name, ".test.js") {
			return nil
		}
		if ext := filepath.Ext(name); ext == ".js" || ext == ".html" {
			rel, err := filepath.Rel(filepath.Join("..", ".."), path)
			if err != nil {
				return err
			}
			sources = append(sources, filepath.ToSlash(rel))
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking %s: %v", extensionDir, err)
	}
	if len(sources) == 0 {
		t.Fatalf("%s holds no .js or .html files, the guard would pass vacuously", extensionDir)
	}
	sort.Strings(sources)

	for _, file := range sources {
		if missing := missingFrom(byTarget, file); len(missing) > 0 {
			t.Errorf("%s is extension source but is not a resource of %v: it would be missing from the "+
				"Safari build, so add it to the Resources phase of both extension targets in %s",
				file, missing, pbxprojPath)
		}
	}
}

// TestSafariTargetsMatchSpecPlatforms is the Linux half of SPEC-0015 REQ
// "Xcode Project Generation": the conversion itself needs a Mac, but two app
// targets that embed an extension, at iOS 15 and macOS 12 floors, do not.
func TestSafariTargetsMatchSpecPlatforms(t *testing.T) {
	p := readPBX(t)

	apps := map[string][]string{}
	for _, target := range p.withISA("PBXNativeTarget") {
		if !strings.Contains(p.value(target, "productType"), "application") {
			continue
		}
		var embedded []string
		for _, phase := range p.list(target, "buildPhases") {
			if p.isa(phase) != "PBXCopyFilesBuildPhase" {
				continue
			}
			for _, buildFile := range p.list(phase, "files") {
				if ref := p.value(buildFile, "fileRef"); strings.HasSuffix(p.value(ref, "path"), ".appex") {
					embedded = append(embedded, p.value(ref, "path"))
				}
			}
		}
		apps[p.value(target, "name")] = embedded
	}
	for _, want := range []string{"joe-links (iOS)", "joe-links (macOS)"} {
		embedded, ok := apps[want]
		if !ok {
			t.Errorf("%s declares no %s target (SPEC-0015 REQ \"Xcode Project Generation\")", pbxprojPath, want)
			continue
		}
		if len(embedded) == 0 {
			t.Errorf("%s embeds no .appex, so Safari would never offer the extension", want)
		}
	}

	floors := map[string][2]int{"IPHONEOS_DEPLOYMENT_TARGET": {15, 0}, "MACOSX_DEPLOYMENT_TARGET": {12, 0}}
	for key, floor := range floors {
		re := regexp.MustCompile(`(?m)^\t*` + key + ` = ([0-9]+)(?:\.([0-9]+))?;`)
		found := 0
		for _, config := range p.withISA("XCBuildConfiguration") {
			m := re.FindStringSubmatch(p.objects[config])
			if m == nil {
				continue
			}
			found++
			major, _ := strconv.Atoi(m[1])
			minor, _ := strconv.Atoi(m[2])
			if major < floor[0] || (major == floor[0] && minor < floor[1]) {
				t.Errorf("%s = %s.%s is below the SPEC-0015 floor of %d.%d",
					key, m[1], m[2], floor[0], floor[1])
			}
		}
		if found == 0 {
			t.Errorf("%s never sets %s, so the SPEC-0015 platform floor is unenforced", pbxprojPath, key)
		}
	}
}

func keysOf(m map[string][]string) []string {
	var keys []string
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func sortedKeys(set map[string]bool) []string {
	var keys []string
	for k := range set {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
