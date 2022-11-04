package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"github.com/thanm/grvutils/zgr"
)

// TODO/FIXME:
// - rewrite to handle import paths outside stdlib
//   + do an initial "go list -json" on tgt to see if stdlib or no
//   + calculate glo (cache key) based on goroot hash and cur dir hash
//   + don't change to goroot each time for go list
// - add build timings (might need to defeat build cache)
// - do "go list" and package size calculations in parallel

var verbflag = flag.Int("v", 0, "Verbose trace output level")
var glcacheflag = flag.String("glcache", "/tmp/.glcache", "cache dir for 'go list' invocations")
var tgtflag = flag.String("tgt", "cmd/buildid", "target to analyze")
var topnflag = flag.Int("topn", 3, "generate DOT graph with top N paths")

// Pkg holds results from "go list -json". There are many more
// fields we could ask for, but at the moment import path is all
// we need.
type Pkg struct {
	ImportPath string
	Imports    []string
}

var curglo string

func glo(goroot string) string {
	cmd := exec.Command("git", "log", "-1", "--oneline")
	cmd.Dir = goroot
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
	if _, err := fmt.Fprintf(outf, "%s\n", curglo); err != nil {
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
	if val != curglo {
		verb(2, "cache mismatch:\ngot %q\nwant %q\n", val, curglo)
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
			verb(2, "cache miss on %s", dir)
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("problems reading cache %s: %v",
			*glcacheflag, err)
	}
	verb(2, "%s cache hit on %s", tag, dir)
	return contents, true, nil
}

func writeCache(dir, tag string, content []byte) error {
	verb(2, "cache write for %s", dir)
	if err := os.WriteFile(cachePath(dir, tag), content, 0777); err != nil {
		return err
	}
	return nil
}

func goList(dir, goroot string) (*Pkg, error) {
	// Try cache first
	var pkg Pkg
	out, valid, err := tryCache(dir, "list")
	if err != nil {
		return nil, err
	} else if !valid {
		// cache miss, run "go list"
		ppath := filepath.Join(goroot, dir)
		cmd := exec.Command("go", "list", "-json", ppath)
		cmd.Dir = ppath
		out, err = cmd.Output()
		if err != nil {
			return nil, fmt.Errorf("go list -json %s: %v", dir, err)
		}
		// write back to cache
		if err := writeCache(dir, "list", out); err != nil {
			return nil, fmt.Errorf("writing cache: %v", err)
		}
	}
	// unpack
	if err := json.Unmarshal(out, &pkg); err != nil {
		return nil, fmt.Errorf("go list -json %s: unmarshal: %v", dir, err)
	}
	return &pkg, nil
}

func (g *pgraph) nidPkgSize(nid string) int {
	nlab := g.LookupNode(nid).Label()
	pkg := nlab[1 : len(nlab)-1]
	sz, _ := pkgSize(pkg, g.goroot)
	return sz
}

func pkgSize(dir, goroot string) (int, error) {
	// Try cache first
	out, valid, err := tryCache(dir, "build")
	if err != nil {
		return 0, err
	} else if !valid {
		// cache miss, run "go build"
		ppath := filepath.Join(goroot, dir)
		outfile := cachePath(dir, "archive")
		os.RemoveAll(outfile)
		cmd := exec.Command("go", "build", "-o", outfile, ppath)
		cmd.Dir = ppath
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
		if err := writeCache(dir, "build", out); err != nil {
			return 0, fmt.Errorf("writing cache: %v", err)
		}
	}
	// unpack
	var sz int
	if n, err := fmt.Sscanf(string(out), "%d", &sz); err != nil || n != 1 {
		return 0, fmt.Errorf("interpreting pksize %s: %v", string(out), err)
	}
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

func popNode(tgt string, g *pgraph) (string, error) {
	verb(2, "=-= popNode(%s)", tgt)
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
	pk, err := goList(tgt, g.goroot)
	if err != nil {
		return snid, err
	}
	for _, dep := range pk.Imports {
		if dep == "unsafe" || dep == "errors" || dep == "tgt" {
			continue
		}
		if _, ok := g.nodes[dep]; !ok {
			if _, err := popNode(dep, g); err != nil {
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
			//"weight": ws,
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

func traceCritical(g *pgraph, rootnid, mptn string, nodes []string, included map[string]bool) error {
	// paint the critical path starting at mptn
	included[rootnid] = true
	cp := []pathsegment{
		pathsegment{
			nid: mptn,
			pkg: g.LookupNode(mptn).Label(),
			wt:  0,
		}}
	cur := mptn
	for cur != rootnid {
		included[cur] = true
		// Look at in-edges.
		n := g.LookupNode(cur)
		bestweight := 0
		var bestpred uint32
		edges := g.GetInEdges(n)
		if len(edges) == 0 {
			return fmt.Errorf("internal error: no-preds node %s", cur)
		}
		for _, e := range edges {
			edge := g.GetEdge(e)
			src, _ := g.GetEndpoints(edge)
			wt, werr := g.EdgeWeight(edge)
			if werr != nil {
				return werr
			}
			if wt > bestweight {
				bestpred = src
				bestweight = wt
			}
		}
		if bestweight == 0 {
			panic("unexpected")
		}
		ps := pathsegment{
			nid: g.GetNode(bestpred).Id(),
			pkg: g.GetNode(bestpred).Label(),
			wt:  bestweight,
		}
		cp = append(cp, ps)
		cur = g.GetNode(bestpred).Id()
	}

	verb(0, "\nCritical path:")
	for i := range cp {
		seg := cp[i]
		sz := g.nidPkgSize(seg.nid)
		verb(0, "%s [wt:%d]", seg.pkg, sz)
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
	for _, v := range listing {
		verb(2, "start walk of %s %s", v, g.LookupNode(v).Label())
		toval := pathto[v]
		n := g.LookupNode(v)
		edges := g.GetInEdges(n)
		for _, e := range edges {
			edge := g.GetEdge(e)
			src, sink := g.GetEndpoints(edge)
			verb(2, "consider edge %d:%d %s -> %s",
				src, sink, g.GetNode(src).Label(), g.GetNode(sink).Label())
			sn := g.GetNode(src)
			srcnid := sn.Id()
			wt, werr := g.EdgeWeight(edge)
			if werr != nil {
				return werr
			}
			srcval := pathto[srcnid] + wt
			if srcval > toval {
				verb(2, "update toval for %s to %d (edge from %s)",
					n.Label(), srcval, sn.Label())
				toval = srcval
			}
		}
		pathto[v] = toval
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
			return di < dj
		})

	// Print for debugging
	verb(1, "nodes with pathto values:")
	for k, v := range nodes {
		sz := g.nidPkgSize(v)
		verb(1, "%d: %s sz=%d pt=%d", k, v, sz, pathto[v])
	}

	for i := 0; i < *topnflag && i < len(nodes); i++ {
		cnidx := len(nodes) - i - 1
		cnid := nodes[cnidx]
		if included[cnid] {
			continue
		}
		verb(1, "traceCritical(root=%s nid=%s)\n", nid, cnid)
		traceCritical(g, nid, cnid, nodes, included)
	}

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
	target := *tgtflag
	gr, err := goRoot()
	if err != nil {
		log.Fatal(err)
	}
	curglo = glo(gr)
	verb(2, "glo: %s", curglo)
	g := &pgraph{
		Graph:  zgr.NewGraph(),
		nodes:  make(map[string]int),
		goroot: gr + "/src",
	}
	nid, perr := popNode(target, g)
	if perr != nil {
		log.Fatal(perr)
	}
	outf, err := os.OpenFile("tmp.dot", os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0644)
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
	gt := g.transpose()
	if err := gt.Write(outf, nidToId(g, included)); err != nil {
		log.Fatal(err)
	}
	fmt.Fprintf(os.Stderr, "%s\n", g.String())
}
