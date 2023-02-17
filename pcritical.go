// Copyright 2023 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"

	"github.com/thanm/gocmdcache"
	"github.com/thanm/grvutils/zgr"
)

// TODO/FIXME:
// - run "go list" calculations for deps in parallel
// - add module path awareness (e.g. don't emit nodes not in main module)
// - add package staleness check
// - add build timings (might need to defeat build cache)

var verbflag = flag.Int("v", 0, "Verbose trace output level")
var glcacheflag = flag.String("glcache", "/tmp/.glcache", "cache dir for 'go list' invocations")
var tgtflag = flag.String("tgt", "", "target to analyze")
var dotoutflag = flag.String("dotout", "tmp.dot", "DOT file to emit")
var nostdflag = flag.Bool("nostd", false, "Ignore stdlib package deps")
var inunsflag = flag.Bool("include-unsafe", false, "include \"unsafe\" package")
var polylineflag = flag.Bool("polyline", false, "Add splines=polyline attribute to generated DOT graph")

// hashes for use with disk cache
var goroothash string
var repohash string

// cache
var gcache *gocmdcache.Cache

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

func goListUncached(tgt string) (*gocmdcache.Pkg, error) {
	// run "go list"
	cmd := exec.Command("go", "list", "-json", tgt)
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("go list -json %s: %v", tgt, err)
	}
	// unpack
	var pkg gocmdcache.Pkg
	if err := json.Unmarshal(out, &pkg); err != nil {
		return nil, fmt.Errorf("go list -json %s: unmarshal: %v", tgt, err)
	}
	return &pkg, nil
}

func goList(dir string) (*gocmdcache.Pkg, error) {
	return gcache.GoList(dir)
}

func (g *pgraph) nidPkgSize(nid string) (gocmdcache.PkgInfo, error) {
	nlab := g.LookupNode(nid).Label()
	pkg := nlab[1 : len(nlab)-1]
	return gcache.PkgSize(pkg)
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
	pk, err := goList(tgt)
	if err != nil {
		return snid, err
	}
	pskip := func(dep string) bool {
		return (!*inunsflag && dep == "unsafe") ||
			dep == "C"
	}

	// first loop to warm pk cache in parallel
	var wg sync.WaitGroup
	wg.Add(len(pk.Imports))
	sema := make(chan struct{}, runtime.GOMAXPROCS(0))
	verb(2, "processing %d deps in parallel for %s", len(pk.Imports), pk.ImportPath)
	for _, dep := range pk.Imports {
		if pskip(dep) {
			wg.Done()
			continue
		}
		go func(pk string) {
			sema <- struct{}{}
			defer func() {
				<-sema
				wg.Done()
			}()
			goList(pk)
		}(dep)
	}
	wg.Wait()
	// second loop to actually build the graph
	for _, dep := range pk.Imports {
		if pskip(dep) {
			continue
		}
		pk, err := goList(dep)
		if err != nil {
			return snid, err
		}
		if *nostdflag && pk.Standard {
			// assumption is that stdlib packages will only
			// depend on other stdlib packages.
			continue
		}
		if _, ok := g.nodes[dep]; !ok {
			if _, err := populateNode(dep, g); err != nil {
				return snid, err
			}
		}
		g.AddEdge(snid, g.snid(dep), nil)
	}
	return snid, nil
}

func (g *pgraph) computeEdgeWeights(rootnid string) error {

	verb(1, "starting pkg size computation root=%s", rootnid)
	verb(2, "g.nodes: %+v", g.nodes)

	// Compute package sizes.
	var wg sync.WaitGroup
	wg.Add(len(g.nodes))
	sema := make(chan struct{}, runtime.GOMAXPROCS(0)/2)
	for pk := range g.nodes {
		go func(pk string) {
			sema <- struct{}{}
			defer func() {
				<-sema
				wg.Done()
			}()
			gcache.PkgSize(pk)
		}(pk)
	}
	wg.Wait()

	verb(1, "finished pkg size computation, applying edge weights")

	// Now use sizes for edge weights.
	for pk := range g.nodes {
		nid := g.snid(pk)
		n := g.LookupNode(nid)
		verb(2, "weight visit %s nid=%s", pk, nid)
		edges := g.GetEdges(n)
		for _, e := range edges {
			edge := g.GetEdge(e)
			src, sink := g.GetEndpoints(edge)
			sinknode := g.GetNode(sink)
			srcnode := g.GetNode(src)
			verb(2, "compute weight for %s->%s p=%s",
				srcnode.Id(), sinknode.Id(), sinknode.Label())
			pi, err := g.nidPkgSize(sinknode.Id())
			if err != nil {
				return fmt.Errorf("bad size calc: %v", err)
			}
			ws := fmt.Sprintf("%d", pi.Size)
			attrs := map[string]string{
				"label": ws,
			}
			g.SetEdgeAttrs(srcnode.Id(), sinknode.Id(), attrs)
		}
	}
	return nil
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

	var sb strings.Builder
	if err := writeCP(&sb, cp, g); err != nil {
		return err
	}
	cps := sb.String()

	// Write CP to cache
	root := cp[0].pkg
	troot := root[1 : len(root)-1]
	if err := gcache.WriteCache(troot, "cpath", []byte(cps)); err != nil {

		return err
	}

	// Also emit CP to stdout.
	fmt.Printf("\nCritical path:\n%s\n", cps)

	// Done
	return nil
}

func writeCP(w io.Writer, cp []pathsegment, g *pgraph) error {
	for i := range cp {
		seg := cp[i]
		pi, err := g.nidPkgSize(seg.nid)
		if err != nil {
			return err
		}
		if _, err := fmt.Fprintf(w, "%s [weight:%d nfuncs:%d]\n",
			seg.pkg, pi.Size, pi.NumFuncs); err != nil {
			return err
		}
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
		var err error
		pi, err := g.nidPkgSize(nid)
		if err != nil {
			return err
		}
		pathto[nid] = pi.Size
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
			pi, err := g.nidPkgSize(srcnid)
			if err != nil {
				return err
			}
			srcwt := pi.Size
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
		pi, err := g.nidPkgSize(v)
		if err != nil {
			return err
		}
		nlab := g.LookupNode(v).Label()
		verb(1, "%d: %s sz=%d nf=%d pt=%d %s",
			k, v, pi.Size, pi.NumFuncs, pathto[v], nlab)
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

	// Run "go list" on the target to establish some basics.
	target := *tgtflag
	verb(1, "target is: %s", *tgtflag)
	pk, err := goListUncached(target)
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
		verb(2, "using goroothash as repohash")
	} else {
		// Collect separate hash from repo.
		repohash = glo(pk.Root, false)
		verb(2, "repohash: %s", repohash)
	}

	// Create cache
	gcache, err = gocmdcache.Make(repohash, goroothash, *glcacheflag, *verbflag)
	if err != nil {
		log.Fatalf("error creating cache: %v", err)
	}

	// Construct dependency graph.
	g := &pgraph{
		Graph:  zgr.NewGraph(),
		nodes:  make(map[string]int),
		goroot: gr + "/src",
	}
	if *polylineflag {
		pla := map[string]string{"splines": "polyline"}
		g.SetAttrs(pla)
	}
	nid, perr := populateNode(target, g)
	if perr != nil {
		log.Fatal(perr)
	}
	if err := g.computeEdgeWeights(nid); err != nil {
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
