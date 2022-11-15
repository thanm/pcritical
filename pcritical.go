package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"

	"github.com/thanm/grvutils/zgr"
)

// TODO/FIXME:
// - add module path awareness (e.g. don't emit nodes not in main module)
// - add package staleness check
// - add build timings (might need to defeat build cache)
// - do "go list" and package size calculations in parallel,
//   and/or deps in parallel

var verbflag = flag.Int("v", 0, "Verbose trace output level")
var glcacheflag = flag.String("glcache", "/tmp/.glcache", "cache dir for 'go list' invocations")
var tgtflag = flag.String("tgt", "", "target to analyze")
var dotoutflag = flag.String("dotout", "tmp.dot", "DOT file to emit")
var nostdflag = flag.Bool("nostd", false, "Ignore stdlib package deps")

// Pkg holds results from "go list -json". There are many more
// fields we could ask for, but at the moment we just need a few.
type Pkg struct {
	Standard   bool
	ImportPath string
	Root       string
	Imports    []string
}

// Cache of "go list" results
var listcache = make(map[string]*Pkg)

// Cache of package sizes from gobuild
var pkgsizecache = make(map[string]int)

// For parallel pkg size computations
var pkgsizewg sync.WaitGroup
var pkgsizesema = make(chan struct{}, runtime.GOMAXPROCS(0))

// hashes for use with disk cache
var goroothash string
var repohash string

func glo(repo string, soft bool) string {
	if soft {
		// Don't fail if no .git, just return path.
		gp := filepath.Join(repo, ".git")
		_, err := os.ReadDir(gp)
		if os.IsNotExist(err) {
			return repo
		}
	}
	cmd := exec.Command("git", "log", "-1", "--oneline")
	cmd.Dir = repo
	out, err := cmd.Output()
	if err != nil {
		log.Fatalf("error running git log -1 --oneline: %v", err)
	}
	return strings.TrimSpace(string(out))
}

const glopath = "=glo="

func initCache() error {
	p := filepath.Join(*glcacheflag, glopath)
	outf, err := os.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0644)
	if err != nil {
		return fmt.Errorf("opening %s: %v", p, err)
	}
	if _, err := fmt.Fprintf(outf, "%s %s\n", repohash, goroothash); err != nil {
		return fmt.Errorf("writing %s: %v", p, err)
	}
	if err := outf.Close(); err != nil {
		return err
	}
	return nil
}

func cacheValid() (bool, error) {
	p := filepath.Join(*glcacheflag, glopath)
	contents, err := os.ReadFile(p)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		} else {
			return false, err
		}
	}
	val := strings.TrimSpace(string(contents))
	want := repohash + " " + goroothash
	if val != want {
		verb(2, "cache mismatch:\ngot %q\nwant %q\n", val, want)
		if err := os.RemoveAll(*glcacheflag); err != nil {
			return false, err
		}
		if err := os.Mkdir(*glcacheflag, 0777); err != nil {
			return false, err
		}
		return false, nil
	}
	return true, nil
}

func cachePath(dir string, tag string) string {
	dtag := strings.ReplaceAll(dir, "/", "%")
	return filepath.Join(*glcacheflag, dtag+"."+tag)
}

func tryCache(dir string, tag string) ([]byte, bool, error) {
	err := os.Mkdir(*glcacheflag, 0777)
	needsinit := false
	if err == nil {
		needsinit = true
	} else if !os.IsExist(err) {
		return nil, false, fmt.Errorf("unable to create cache %s: %v",
			*glcacheflag, err)
	}
	if isvalid, err := cacheValid(); err != nil {
		return nil, false, fmt.Errorf("problems reading cache %s: %v",
			*glcacheflag, err)
	} else if !isvalid {
		needsinit = true
	}
	if needsinit {
		if err = initCache(); err != nil {
			return nil, false, err
		}
	}
	contents, err := os.ReadFile(cachePath(dir, tag))
	if err != nil {
		if os.IsNotExist(err) {
			verb(3, "%s cache miss on %s", tag, dir)
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("problems reading cache %s: %v",
			*glcacheflag, err)
	}
	verb(3, "%s cache hit on %s", tag, dir)
	return contents, true, nil
}

func writeCache(dir, tag string, content []byte) error {
	verb(2, "cache write for %s", dir)
	if err := os.WriteFile(cachePath(dir, tag), content, 0777); err != nil {
		return err
	}
	return nil
}

func goList(dir string) (*Pkg, error) {
	// Try mem cache first
	if cpk, ok := listcache[dir]; ok {
		return cpk, nil
	}
	// Try disk cache next
	var pkg Pkg
	out, valid, err := tryCache(dir, "list")
	if err != nil {
		return nil, err
	} else if !valid {
		// cache miss, run "go list"
		pk, out, err := goListUncached(dir, "")
		if err != nil {
			return nil, err
		}
		listcache[dir] = pk
		// write back to cache
		if err := writeCache(dir, "list", out); err != nil {
			return nil, fmt.Errorf("writing cache: %v", err)
		}
		return pk, nil
	}
	// unpack
	if err := json.Unmarshal(out, &pkg); err != nil {
		return nil, fmt.Errorf("go list -json %s: unmarshal: %v", dir, err)
	}
	listcache[dir] = &pkg
	return &pkg, nil
}

func goListUncached(tgt, dir string) (*Pkg, []byte, error) {
	// run "go list"
	cmd := exec.Command("go", "list", "-json", tgt)
	if dir != "" {
		cmd.Dir = dir
	}
	out, err := cmd.Output()
	if err != nil {
		return nil, nil, fmt.Errorf("go list -json %s: %v", dir, err)
	}
	// unpack
	var pkg Pkg
	if err := json.Unmarshal(out, &pkg); err != nil {
		return nil, nil, fmt.Errorf("go list -json %s: unmarshal: %v", dir, err)
	}
	return &pkg, out, nil
}

func (g *pgraph) nidPkgSize(nid string) int {
	nlab := g.LookupNode(nid).Label()
	pkg := nlab[1 : len(nlab)-1]
	sz, _ := pkgSize(pkg, g.goroot)
	return sz
}

func pkgSize(dir, goroot string) (int, error) {
	// Try mem cache first
	if v, ok := pkgsizecache[dir]; ok {
		return v, nil
	}
	// Try disk cache next
	out, valid, err := tryCache(dir, "build")
	if err != nil {
		return 0, err
	} else if !valid {
		// cache miss, run "go build"
		outfile := cachePath(dir, "archive")
		os.RemoveAll(outfile)
		verb(2, "build cmd is 'go build -o %s %s", outfile, dir)
		cmd := exec.Command("go", "build", "-o", outfile, dir)
		out, err = cmd.CombinedOutput()
		if err != nil {
			return 0, fmt.Errorf("go build %s: %v", dir, err)
		}
		fi, err := os.Stat(outfile)
		if err != nil {
			return 0, fmt.Errorf("stat on %s: %v", outfile, err)
		}
		sout := fmt.Sprintf("%d\n", fi.Size())
		out = []byte(sout)
		// write back to cache
		pkgsizecache[dir] = int(fi.Size())
		if err := writeCache(dir, "build", out); err != nil {
			return 0, fmt.Errorf("writing cache: %v", err)
		}
	}
	// unpack
	var sz int
	if n, err := fmt.Sscanf(string(out), "%d", &sz); err != nil || n != 1 {
		return 0, fmt.Errorf("interpreting pksize %s: %v", string(out), err)
	}
	pkgsizecache[dir] = sz

	return sz, nil
}

func goRoot() (string, error) {
	cmd := exec.Command("go", "env", "GOROOT")
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

func nodeAttr(n string) map[string]string {
	qu := "\""
	return map[string]string{
		"label": qu + n + qu,
	}
}

type pgraph struct {
	*zgr.Graph
	nodes  map[string]int
	tslist []string
	goroot string
}

func (g *pgraph) transpose() *pgraph {
	return &pgraph{
		Graph:  g.Transpose(),
		nodes:  g.nodes,
		goroot: g.goroot,
	}
}

func tsvisit(g *pgraph, snid string, visited map[string]bool) {
	if visited[snid] {
		return
	}
	visited[snid] = true
	n := g.LookupNode(snid)
	edges := g.GetEdges(n)
	for _, e := range edges {
		edge := g.GetEdge(e)
		_, sink := g.GetEndpoints(edge)
		sn := g.GetNode(sink)
		tsvisit(g, sn.Id(), visited)
	}
	g.tslist = append(g.tslist, snid)
}

func topsort(g *pgraph, root string) []string {
	visited := make(map[string]bool)
	tsvisit(g, root, visited)
	n := len(g.tslist)
	final := make([]string, n)
	for k := range g.tslist {
		final[n-k-1] = g.tslist[k]
	}
	g.tslist = nil
	return final
}

func (g *pgraph) nid(n string) int {
	if val, ok := g.nodes[n]; !ok {
		panic("bad")
	} else {
		return val
	}
}

func (g *pgraph) snid(n string) string {
	return fmt.Sprintf("N%d", g.nid(n))
}

func populateNode(tgt string, g *pgraph) (string, error) {
	verb(2, "=-= populateNode(%s)", tgt)
	if _, ok := g.nodes[tgt]; ok {
		panic("bad")
	}
	nid := len(g.nodes)
	g.nodes[tgt] = nid
	snid := g.snid(tgt)
	if err := g.MakeNode(snid, nodeAttr(tgt)); err != nil {
		return snid, err
	}
	// add edges to deps
	pk, err := goList(tgt)
	if err != nil {
		return snid, err
	}
	for _, dep := range pk.Imports {
		if dep == "unsafe" || dep == "C" {
			continue
		}
		if *nostdflag {
			pk, err := goList(dep)
			if err != nil {
				return snid, err
			}
			if pk.Standard {
				continue
			}
		}
		if _, ok := g.nodes[dep]; !ok {
			if _, err := populateNode(dep, g); err != nil {
				return snid, err
			}
		}
		verb(2, "grabbing pk size for %s", dep)
		weight, err := pkgSize(dep, g.goroot)
		if err != nil {
			return "", err
		}
		ws := fmt.Sprintf("%d", weight)
		attrs := map[string]string{
			"label": ws,
		}
		g.AddEdge(snid, g.snid(dep), attrs)
	}
	return snid, nil
}

func (g *pgraph) EdgeWeight(e *zgr.Edge) (int, error) {
	eattrs := g.GetEdgeAttrs(e)
	var wt int
	if n, err := fmt.Sscanf(eattrs["label"], "%d", &wt); err != nil || n != 1 {
		src, sink := g.GetEndpoints(e)
		return 0, fmt.Errorf("can't find label on edge %d->%d", src, sink)
	}
	return wt, nil
}

type pathsegment struct {
	nid string
	pkg string
	wt  int
}

func traceCritical(g *pgraph, rootnid string, nodes []string, included map[string]bool, pathto map[string]int) error {
	// paint the critical path starting at root
	included[rootnid] = true
	cp := []pathsegment{
		pathsegment{
			nid: rootnid,
			pkg: g.LookupNode(rootnid).Label(),
			wt:  0,
		}}
	cur := rootnid
	for {
		included[cur] = true
		// Look at out-edges.
		n := g.LookupNode(cur)
		edges := g.GetEdges(n)
		if len(edges) == 0 {
			break
		}
		var bestsucc string
		var bestpt int
		var bestwt int
		var attrs map[string]string
		for _, e := range edges {
			edge := g.GetEdge(e)
			_, sink := g.GetEndpoints(edge)
			sinknid := g.GetNode(sink).Id()
			sinkpt := pathto[sinknid]
			wt, werr := g.EdgeWeight(edge)
			if werr != nil {
				return werr
			}
			if bestpt < sinkpt {
				bestpt = sinkpt
				bestsucc = sinknid
				bestwt = wt
				attrs = g.GetEdgeAttrs(edge)
			}
		}
		if bestpt == 0 {
			panic("unexpected")
		}
		// paint edge
		attrs["color"] = "red"
		g.SetEdgeAttrs(n.Id(), bestsucc, attrs)
		// add segment
		ps := pathsegment{
			nid: bestsucc,
			pkg: g.LookupNode(bestsucc).Label(),
			wt:  bestwt,
		}
		cp = append(cp, ps)
		cur = g.LookupNode(bestsucc).Id()
	}

	verb(0, "\nCritical path:")
	for i := range cp {
		seg := cp[i]
		sz := g.nidPkgSize(seg.nid)
		pt := pathto[seg.nid]
		verb(0, "%s [weight:%d pathto:%d]", seg.pkg, sz, pt)
	}
	return nil
}

// markCriticalPaths picks out N critical paths in the graph, prints them
// out, and updates the graph edge attributes. This version uses
// weighted edges, where the weight from X->Y is considered to be the
// estimated build time of Y.
func markCriticalPaths(g *pgraph, nid string, included map[string]bool) error {
	listing := topsort(g, nid)

	verb(2, "topsorted listing: %+v", listing)

	pathto := make(map[string]int)
	for _, nid := range listing {
		pathto[nid] = g.nidPkgSize(nid)
	}
	for k := range listing {
		nid := listing[len(listing)-k-1]
		n := g.LookupNode(nid)
		verb(2, "start walk of %s %s", nid, n.Label())
		toval := pathto[nid]
		edges := g.GetInEdges(n)
		for _, e := range edges {
			edge := g.GetEdge(e)
			src, _ := g.GetEndpoints(edge)
			srcnode := g.GetNode(src)
			srcnid := srcnode.Id()
			verb(2, "consider edge %s -> %s",
				g.GetNode(src).Label(), n.Label())
			srcwt := g.nidPkgSize(srcnid)
			npt := toval + srcwt
			if pathto[srcnid] < npt {
				verb(2, "update pathto[%s] to %d (edge to %s)",
					srcnode.Label(), npt, n.Label())
				pathto[srcnid] = npt
			}
		}
	}

	// Sort nodes by pathto.
	nodes := make([]string, 0, len(pathto))
	for k := range pathto {
		nodes = append(nodes, k)
	}
	sort.SliceStable(nodes,
		func(i, j int) bool {
			di := pathto[nodes[i]]
			dj := pathto[nodes[j]]
			return dj < di
		})

	// Print for debugging
	verb(1, "nodes with pathto values:")
	for k, v := range nodes {
		sz := g.nidPkgSize(v)
		nlab := g.LookupNode(v).Label()
		verb(1, "%d: %s sz=%d pt=%d %s", k, v, sz, pathto[v], nlab)
	}

	// trace critical path
	traceCritical(g, nid, nodes, included, pathto)

	return nil
}

func nidToId(g *pgraph, m map[string]bool) map[uint32]bool {
	res := make(map[uint32]bool)
	for k := range m {
		node := g.LookupNode(k)
		if node == nil {
			panic("nil node in nidToId")
		}
		res[node.Idx()] = true
	}
	return res
}

func verb(vlevel int, s string, a ...interface{}) {
	if *verbflag >= vlevel {
		fmt.Printf(s, a...)
		fmt.Printf("\n")
	}
}
func usage(msg string) {
	if len(msg) > 0 {
		fmt.Fprintf(os.Stderr, "error: %s\n", msg)
	}
	fmt.Fprintf(os.Stderr, "usage: pcritical [flags]\n")
	flag.PrintDefaults()
	os.Exit(2)
}

func main() {
	log.SetFlags(0)
	log.SetPrefix("pcritical: ")
	flag.Parse()
	if *tgtflag == "" {
		usage("supply target with -tgt flag")
	}

	// Collect GOROOT as a first step.
	gr, err := goRoot()
	verb(1, "GOROOT is %s", gr)
	if err != nil {
		log.Fatal(err)
	}

	// Run "go list" on the target without any caching, just so that
	// we can establish some basics.
	target := *tgtflag
	verb(1, "target is: %s", *tgtflag)
	pk, _, err := goListUncached(target, "")
	if err != nil {
		log.Fatal(err)
	}
	verb(2, "pkg: %+v", *pk)

	// Examine goroot, collect current git hash if applicable.
	goroothash = glo(gr, true)
	verb(2, "goroothash: %s", goroothash)

	if pk.Root == gr {
		// If pk root is the same as target root, we're analyzing something
		// in the standard library, so repo hash is goroot hash.
		repohash = goroothash
	} else {
		// Collect separate hash from repo.
		repohash = glo(pk.Root, false)
		verb(2, "repohash: %s", repohash)
	}

	// Construct dependency graph.
	g := &pgraph{
		Graph:  zgr.NewGraph(),
		nodes:  make(map[string]int),
		goroot: gr + "/src",
	}
	nid, perr := populateNode(target, g)
	if perr != nil {
		log.Fatal(perr)
	}
	fmt.Printf("... creating DOT file %s\n", *dotoutflag)
	outf, err := os.OpenFile(*dotoutflag, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0644)
	if err != nil {
		log.Fatal(err)
	}
	defer func() {
		if err := outf.Close(); err != nil {
			log.Fatal(err)
		}
	}()
	included := make(map[string]bool)
	if err := markCriticalPaths(g, nid, included); err != nil {
		log.Fatal(err)
	}
	if err := g.Write(outf, nidToId(g, included)); err != nil {
		log.Fatal(err)
	}
	verb(1, "graph:\n%s", g.String())
}
