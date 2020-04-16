// Package runner implements a go/analysis runner. It makes heavy use
// of on-disk caching to reduce overall memory usage and to speed up
// repeat runs.
//
// Public API
//
// A Runner maps a list of analyzers and package patterns to a list of
// results. Results provide access to diagnostics, directives, errors
// encountered, and information about packages. Results explicitly do
// not contain ASTs or type information. All position information is
// returned in the form of token.Position, not token.Pos. All work
// that requires access to the loaded representation of a package has
// to occur inside analyzers.
//
// Planning and execution
//
// Analyzing packages is split into two phases: planning and
// execution.
//
// During planning, a directed acyclic graph of package dependencies
// is computed. We materialize the full graph so that we can execute
// the graph from the bottom up, without keeping unnecessary data in
// memory during a DFS and with simplified parallel execution.
//
// During execution, leaf nodes (nodes with no outstanding
// dependencies) get executed in parallel, bounded by a semaphore
// sized according to the number of CPUs. Conceptually, this happens
// in a loop, processing new leaf nodes as they appear, until no more
// nodes are left. In the actual implementation, nodes know their
// dependents, and the last dependency of a node to be processed is
// responsible for scheduling its dependent.
//
// The graph is rooted at a synthetic root node. Upon execution of the
// root node, the algorithm terminates.
//
// Analyzing a package repeats the same planning + execution steps,
// but this time on a graph of analyzers for the package. Parallel
// execution of individual analyzers is bounded by the same semaphore
// as executing packages.
//
// Parallelism
//
// Actions are executed in parallel where the dependency graph allows.
// Overall parallelism is bounded by a semaphore, sized according to
// runtime.NumCPU(). Each concurrently processed package takes up a
// token, as does each analyzer – but a package can always execute at
// least one analyzer, using the package's token.
//
// Depending on the overall shape of the graph, there may be NumCPU
// packages running a single analyzer each, a single package running
// NumCPU analyzers, or anything in between.
//
// Total memory consumption grows roughly linearly with the number of
// CPUs, while total execution time is inversely proportional to the
// number of CPUs. Overall, parallelism is affected by the shape of
// the dependency graph. A lot of inter-connected packages will see
// less parallelism than a lot of independent packages.
//
// Caching
//
// The runner caches facts, directives and diagnostics in a
// content-addressable cache that is designed after Go's own cache.
// Additionally, it makes use of Go's export data.
//
// This cache not only speeds up repeat runs, it also reduces peak
// memory usage. When we've analyzed a package, we cache the results
// and drop them from memory. When a dependent needs any of this
// information, or when analysis is complete and we wish to render the
// results, the data gets loaded from disk again.
//
// Data only exists in memory when it is immediately needed, not
// retained for possible future uses. This trades increased CPU usage
// for reduced memory usage. A single dependency may be loaded many
// times over, but it greatly reduces peak memory usage, as an
// arbitrary amount of time may pass between analyzing a dependency
// and its dependent, during which other packages will be processed.
package runner

// OPT(dh): we could reduce disk storage usage of cached data by
// compressing it, either directly at the cache layer, or by feeding
// compressed data to the cache. Of course doing so may negatively
// affect CPU usage, and there are lower hanging fruit, such as
// needing to cache less data in the first place.

// OPT(dh): right now, each package is analyzed completely
// independently. Each package loads all of its dependencies from
// export data and cached facts. If we have two packages A and B,
// which both depend on C, and which both get analyzed in parallel,
// then C will be loaded twice. This wastes CPU time and memory. It
// would be nice if we could reuse a single C for the analysis of both
// A and B.
//
// We can't reuse the actual types.Package or facts, because each
// package gets its own token.FileSet. Sharing a global FileSet has
// several drawbacks, including increased memory usage and running the
// risk of running out of FileSet address space.
//
// We could however avoid loading the same raw export data from disk
// twice, as well as deserializing gob data twice. One possible
// solution would be a duplicate-suppressing in-memory cache that
// caches data for a limited amount of time. When the same package
// needs to be loaded twice in close succession, we can reuse work,
// without holding unnecessary data in memory for an extended period
// of time.
//
// We would likely need to do extensive benchmarking to figure out how
// long to keep data around to find a sweetspot where we reduce CPU
// load without increasing memory usage.
//
// We can probably populate the cache after we've analyzed a package,
// on the assumption that it will have to be loaded again in the near
// future.

import (
	"bytes"
	"encoding/gob"
	"fmt"
	"go/token"
	"go/types"
	"io"
	"os"
	"reflect"
	"runtime"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	"honnef.co/go/tools/config"
	"honnef.co/go/tools/facts"
	"honnef.co/go/tools/internal/cache"
	tsync "honnef.co/go/tools/internal/sync"
	"honnef.co/go/tools/loader"
	"honnef.co/go/tools/report"
	"honnef.co/go/tools/unused"

	"golang.org/x/tools/go/analysis"
	"golang.org/x/tools/go/packages"
	"golang.org/x/tools/go/types/objectpath"
)

type Diagnostic struct {
	Position token.Position
	End      token.Position
	Category string
	Message  string

	SuggestedFixed []SuggestedFix
	Related        []RelatedInformation
}

// RelatedInformation provides additional context for a diagnostic.
type RelatedInformation struct {
	Position token.Position
	End      token.Position
	Message  string
}

type SuggestedFix struct {
	Message   string
	TextEdits []TextEdit
}

type TextEdit struct {
	Position token.Position
	End      token.Position
	NewText  []byte
}

// A Result describes the result of analyzing a single package.
//
// It holds references to cached diagnostics and directives. They can
// be loaded on demand with Diagnostics and Directives respectively.
type Result struct {
	Package *loader.PackageSpec
	Config  config.Config

	Failed bool
	Errors []error
	// Action results, paths to files
	diagnostics string
	directives  string
	unused      string
}

// Diagnostics loads and returns the diagnostics found while analyzing
// the package.
func (r Result) Diagnostics() ([]Diagnostic, error) {
	if r.Failed {
		panic("Diagnostics called on failed Result")
	}
	if r.diagnostics == "" {
		// this package was only a dependency
		return nil, nil
	}
	var diags []Diagnostic
	f, err := os.Open(r.diagnostics)
	if err != nil {
		return nil, fmt.Errorf("failed loading diagnostics: %w", err)
	}
	defer f.Close()
	dec := gob.NewDecoder(f)
	for {
		var diag Diagnostic
		err := dec.Decode(&diag)
		if err != nil {
			if err == io.EOF {
				break
			}
			return nil, fmt.Errorf("failed loading diagnostics: %w", err)
		}
		diags = append(diags, diag)
	}
	return diags, nil
}

// Directives loads and returns the directives found while analyzing
// the package.
func (r Result) Directives() ([]facts.SerializedDirective, error) {
	if r.Failed {
		panic("Directives called on failed Result")
	}
	if r.directives == "" {
		// this package was only a dependency
		return nil, nil
	}
	var dirs []facts.SerializedDirective
	f, err := os.Open(r.directives)
	if err != nil {
		return nil, fmt.Errorf("failed loading directives: %w", err)
	}
	defer f.Close()
	if err := gob.NewDecoder(f).Decode(&dirs); err != nil {
		return nil, fmt.Errorf("failed loading directives: %w", err)
	}
	return dirs, nil
}

func (r Result) Unused() (unused.SerializedResult, error) {
	if r.Failed {
		panic("Unused called on failed Result")
	}
	if r.unused == "" {
		// this package was only a dependency
		return unused.SerializedResult{}, nil
	}
	var res unused.SerializedResult
	f, err := os.Open(r.unused)
	if err != nil {
		return unused.SerializedResult{}, fmt.Errorf("failed loading unused: %w", err)
	}
	defer f.Close()
	if err := gob.NewDecoder(f).Decode(&res); err != nil {
		return unused.SerializedResult{}, fmt.Errorf("failed loading unused: %w", err)
	}
	return res, nil
}

type action interface {
	Deps() []action
	Triggers() []action
	DecrementPending() bool
	MarkFailed()
	IsFailed() bool
	AddError(error)
}

type baseAction struct {
	// Action description

	deps     []action
	triggers []action
	pending  uint32

	// Action results

	// failed is set to true if the action couldn't be processed. This
	// may either be due to an error specific to this action, in
	// which case the errors field will be populated, or due to a
	// dependency being marked as failed, in which case errors will be
	// empty.
	failed bool
	errors []error
}

func (act *baseAction) Deps() []action     { return act.deps }
func (act *baseAction) Triggers() []action { return act.triggers }
func (act *baseAction) DecrementPending() bool {
	return atomic.AddUint32(&act.pending, ^uint32(0)) == 0
}
func (act *baseAction) MarkFailed()        { act.failed = true }
func (act *baseAction) IsFailed() bool     { return act.failed }
func (act *baseAction) AddError(err error) { act.errors = append(act.errors, err) }

// packageAction describes the act of loading a package, fully
// analyzing it, and storing the results.
type packageAction struct {
	baseAction

	// Action description

	Package   *loader.PackageSpec
	factsOnly bool
	hash      cache.ActionID

	// Action results

	cfg         config.Config
	vetx        string
	directives  string
	diagnostics string
	unused      string
}

func (act *packageAction) String() string {
	return fmt.Sprintf("packageAction(%s)", act.Package)
}

type objectFactKey struct {
	Obj  types.Object
	Type reflect.Type
}

type packageFactKey struct {
	Pkg  *types.Package
	Type reflect.Type
}

type gobFact struct {
	PkgPath string
	ObjPath string
	Fact    analysis.Fact
}

// analyzerAction describes the act of analyzing a package with a
// single analyzer.
type analyzerAction struct {
	baseAction

	// Action description

	Analyzer *analysis.Analyzer

	// Action results

	// We can store actual results here without worrying about memory
	// consumption because analyzer actions get garbage collected once
	// a package has been fully analyzed.
	Result       interface{}
	Diagnostics  []analysis.Diagnostic
	ObjectFacts  map[objectFactKey]analysis.Fact
	PackageFacts map[packageFactKey]analysis.Fact
	Pass         *analysis.Pass
}

func (act *analyzerAction) String() string {
	return fmt.Sprintf("analyzerAction(%s)", act.Analyzer)
}

// A Runner executes analyzers on packages.
type Runner struct {
	Stats     Stats
	GoVersion int

	// Config that gets merged with per-package configs
	cfg       config.Config
	cache     *cache.Cache
	semaphore tsync.Semaphore
}

type subrunner struct {
	*Runner
	analyzers     []*analysis.Analyzer
	analyzerNames string
}

// New returns a new Runner.
func New(cfg config.Config) (*Runner, error) {
	cache, err := cache.Default()
	if err != nil {
		return nil, err
	}

	return &Runner{
		cfg:       cfg,
		cache:     cache,
		semaphore: tsync.NewSemaphore(runtime.NumCPU()),
	}, nil
}

func newSubrunner(r *Runner, analyzers []*analysis.Analyzer) *subrunner {
	analyzerNames := make([]string, len(analyzers))
	for i, a := range analyzers {
		analyzerNames[i] = a.Name
	}
	sort.Strings(analyzerNames)
	return &subrunner{
		Runner:        r,
		analyzers:     analyzers,
		analyzerNames: strings.Join(analyzerNames, ","),
	}
}

func newPackageActionRoot(pkg *loader.PackageSpec, cache map[*loader.PackageSpec]*packageAction) *packageAction {
	a := newPackageAction(pkg, cache)
	a.factsOnly = false
	return a
}

func newPackageAction(pkg *loader.PackageSpec, cache map[*loader.PackageSpec]*packageAction) *packageAction {
	if a, ok := cache[pkg]; ok {
		return a
	}

	a := &packageAction{
		Package:   pkg,
		factsOnly: true, // will be overwritten by any call to Action
	}
	cache[pkg] = a

	// OPT(dh): pre-allocate a.errors
	if len(pkg.Errors) > 0 {
		for _, err := range pkg.Errors {
			a.errors = append(a.errors, err)
		}
		a.failed = true

		// We don't need to process our imports if this package is
		// already broken.
		return a
	}

	// OPT(dh): pre-allocate a.deps
	for _, dep := range pkg.Imports {
		if dep.PkgPath == "unsafe" {
			continue
		}
		depa := newPackageAction(dep, cache)
		depa.triggers = append(depa.triggers, a)
		a.deps = append(a.deps, depa)

		if depa.failed {
			a.failed = true
		}
	}
	// sort dependencies because the list of dependencies is part of
	// the cache key
	sort.Slice(a.deps, func(i, j int) bool {
		return a.deps[i].(*packageAction).Package.ID < a.deps[j].(*packageAction).Package.ID
	})

	a.pending = uint32(len(a.deps))

	return a
}

func newAnalyzerAction(an *analysis.Analyzer, cache map[*analysis.Analyzer]*analyzerAction) *analyzerAction {
	if a, ok := cache[an]; ok {
		return a
	}

	a := &analyzerAction{
		Analyzer:     an,
		ObjectFacts:  map[objectFactKey]analysis.Fact{},
		PackageFacts: map[packageFactKey]analysis.Fact{},
	}
	cache[an] = a
	for _, dep := range an.Requires {
		depa := newAnalyzerAction(dep, cache)
		depa.triggers = append(depa.triggers, a)
		a.deps = append(a.deps, depa)
	}
	a.pending = uint32(len(a.deps))
	return a
}

func getCachedFiles(cache *cache.Cache, ids []cache.ActionID, out []*string) error {
	for i, id := range ids {
		var err error
		*out[i], _, err = cache.GetFile(id)
		if err != nil {
			return err
		}
	}
	return nil
}

func (r *subrunner) do(act action) error {
	a := act.(*packageAction)
	defer func() {
		r.Stats.finishPackage()
		if !a.factsOnly {
			r.Stats.finishInitialPackage()
		}
	}()

	// compute hash of action
	a.cfg = a.Package.Config.Merge(r.cfg)
	h := cache.NewHash("staticcheck " + a.Package.PkgPath)

	// Note that we do not filter the list of analyzers by the
	// package's configuration. We don't allow configuration to
	// accidentally break dependencies between analyzers, and it's
	// easier to always run all checks and filter the output. This
	// also makes cached data more reusable.

	// OPT(dh): not all changes in configuration invalidate cached
	// data. specifically, when a.factsOnly == true, we only care
	// about checks that produce facts, and settings that affect those
	// checks.

	// Config used for constructing the hash; this config doesn't have
	// Checks populated, because we always run all checks.
	hashCfg := a.cfg
	hashCfg.Checks = nil
	// note that we don't hash staticcheck's version; it is set as the
	// salt by a package main.
	fmt.Fprintf(h, "cfg %#v\n", hashCfg)
	fmt.Fprintf(h, "pkg %x\n", a.Package.Hash)
	fmt.Fprintf(h, "analyzers %s\n", r.analyzerNames)
	fmt.Fprintf(h, "go 1.%d\n", r.GoVersion)

	// OPT(dh): do we actually need to hash vetx? can we not assume
	// that for identical inputs, staticcheck will produce identical
	// vetx?
	for _, dep := range a.deps {
		dep := dep.(*packageAction)
		vetxHash, err := cache.FileHash(dep.vetx)
		if err != nil {
			return fmt.Errorf("failed computing hash: %w", err)
		}
		fmt.Fprintf(h, "vetout %q %x\n", dep.Package.PkgPath, vetxHash)
	}
	a.hash = cache.ActionID(h.Sum())

	// try to fetch hashed data
	ids := make([]cache.ActionID, 0, 4)
	ids = append(ids, cache.Subkey(a.hash, "vetx"))
	if !a.factsOnly {
		ids = append(ids,
			cache.Subkey(a.hash, "directives"),
			cache.Subkey(a.hash, "diagnostics"),
			// OPT(dh): only load "unused" data if we're running the U1000 analyzer
			cache.Subkey(a.hash, "unused"),
		)
	}
	if err := getCachedFiles(r.cache, ids, []*string{&a.vetx, &a.directives, &a.diagnostics, &a.unused}); err != nil {
		result, err := r.doUncached(a)
		if err != nil {
			return err
		}
		if a.failed {
			return nil
		}

		// OPT(dh): doUncached returns facts in one format, only for
		// us to immediately convert them to another format.

		// OPT(dh) instead of collecting all object facts and encoding
		// them after analysis finishes, we could encode them as we
		// go. however, that would require some locking.
		gobFacts := &bytes.Buffer{}
		enc := gob.NewEncoder(gobFacts)
		for _, f := range result.objFacts {
			objPath, err := objectpath.For(f.Object)
			if err != nil {
				continue
			}
			gf := gobFact{
				PkgPath: f.Object.Pkg().Path(),
				ObjPath: string(objPath),
				Fact:    f.Fact,
			}
			if err := enc.Encode(gf); err != nil {
				return fmt.Errorf("failed gob encoding data: %w", err)
			}
		}
		for _, f := range result.pkgFacts {
			gf := gobFact{
				PkgPath: f.Package.Path(),
				Fact:    f.Fact,
			}
			if err := enc.Encode(gf); err != nil {
				return fmt.Errorf("failed gob encoding data: %w", err)
			}
		}

		// OPT(dh): We could sort gobFacts for more consistent output,
		// but it doesn't matter. The hash of a package includes all
		// of its files, so whether the vetx hash changes or not, a
		// change to a package requires re-analyzing all dependents,
		// even if the vetx data stayed the same. See also the note at
		// the top of loader/hash.go.

		a.vetx, err = r.writeCache(a, "vetx", gobFacts.Bytes())
		if err != nil {
			return err
		}

		dirs := make([]facts.SerializedDirective, len(result.dirs))
		for i, dir := range result.dirs {
			dirs[i] = facts.SerializeDirective(dir, result.lpkg.Fset)
		}
		a.directives, err = r.writeCacheGob(a, "directives", dirs)
		if err != nil {
			return err
		}

		gobDiags := &bytes.Buffer{}
		enc = gob.NewEncoder(gobDiags)
		for _, diag := range result.diags {
			d := Diagnostic{
				Position: report.DisplayPosition(result.lpkg.Fset, diag.Pos),
				End:      report.DisplayPosition(result.lpkg.Fset, diag.End),
				Category: diag.Category,
				Message:  diag.Message,
			}
			for _, sugg := range diag.SuggestedFixes {
				s := SuggestedFix{
					Message: sugg.Message,
				}
				for _, edit := range sugg.TextEdits {
					s.TextEdits = append(s.TextEdits, TextEdit{
						Position: report.DisplayPosition(result.lpkg.Fset, edit.Pos),
						End:      report.DisplayPosition(result.lpkg.Fset, edit.End),
						NewText:  edit.NewText,
					})
				}
				d.SuggestedFixed = append(d.SuggestedFixed, s)
			}
			for _, rel := range diag.Related {
				d.Related = append(d.Related, RelatedInformation{
					Position: report.DisplayPosition(result.lpkg.Fset, rel.Pos),
					End:      report.DisplayPosition(result.lpkg.Fset, rel.End),
					Message:  rel.Message,
				})
			}
			if err := enc.Encode(d); err != nil {
				return fmt.Errorf("failed gob encoding data: %w", err)
			}
		}
		a.diagnostics, err = r.writeCache(a, "diagnostics", gobDiags.Bytes())
		if err != nil {
			return err
		}

		a.unused, err = r.writeCacheGob(a, "unused", result.unused)
		if err != nil {
			return err
		}
	}
	return nil
}

// ActiveWorkers returns the number of currently running workers.
func (r *Runner) ActiveWorkers() int {
	return r.semaphore.Len()
}

// TotalWorkers returns the maximum number of possible workers.
func (r *Runner) TotalWorkers() int {
	return r.semaphore.Cap()
}

func (r *Runner) writeCache(a *packageAction, kind string, data []byte) (string, error) {
	h := cache.Subkey(a.hash, kind)
	if err := r.cache.PutBytes(h, data); err != nil {
		return "", fmt.Errorf("failed caching data: %w", err)
	}
	// OPT(dh): change PutBytes signature so we get the file name right away, not requiring a call to GetFile
	f, _, err := r.cache.GetFile(h)
	if err != nil {
		return "", fmt.Errorf("failed finding cache entry: %w", err)
	}
	return f, nil
}

func (r *Runner) writeCacheGob(a *packageAction, kind string, data interface{}) (string, error) {
	buf := bytes.NewBuffer(nil)
	if err := gob.NewEncoder(buf).Encode(data); err != nil {
		return "", fmt.Errorf("failed gob encoding data: %w", err)
	}
	return r.writeCache(a, kind, buf.Bytes())
}

type packageActionResult struct {
	objFacts []analysis.ObjectFact
	pkgFacts []analysis.PackageFact
	diags    []analysis.Diagnostic
	unused   unused.SerializedResult
	dirs     []facts.Directive
	lpkg     *loader.Package
}

func (r *subrunner) doUncached(a *packageAction) (packageActionResult, error) {
	// OPT(dh): for a -> b; c -> b; if both a and b are being
	// processed concurrently, we shouldn't load b's export data
	// twice.

	pkg, _, err := loader.Load(a.Package)
	if err != nil {
		return packageActionResult{}, err
	}

	if len(pkg.Errors) > 0 {
		// this handles errors that occured during type-checking the
		// package in loader.Load
		for _, err := range pkg.Errors {
			a.errors = append(a.errors, err)
		}
		a.failed = true
		return packageActionResult{}, nil
	}

	// OPT(dh): instead of parsing directives twice (twice because
	// U1000 depends on the facts.Directives analyzer), reuse the
	// existing result
	dirs := facts.ParseDirectives(pkg.Syntax, pkg.Fset)
	res, err := r.runAnalyzers(a, pkg)

	return packageActionResult{
		objFacts: res.objFacts,
		pkgFacts: res.pkgFacts,
		diags:    res.diagnostics,
		unused:   res.unused,
		dirs:     dirs,
		lpkg:     pkg,
	}, err
}

func pkgPaths(root *types.Package) map[string]*types.Package {
	out := map[string]*types.Package{}
	var dfs func(*types.Package)
	dfs = func(pkg *types.Package) {
		if _, ok := out[pkg.Path()]; ok {
			return
		}
		out[pkg.Path()] = pkg
		for _, imp := range pkg.Imports() {
			dfs(imp)
		}
	}
	dfs(root)
	return out
}

func (r *Runner) loadFacts(root *types.Package, dep *packageAction, objFacts map[objectFactKey]analysis.Fact, pkgFacts map[packageFactKey]analysis.Fact) error {
	// Load facts of all imported packages
	vetx, err := os.Open(dep.vetx)
	if err != nil {
		return fmt.Errorf("failed loading cached facts: %w", err)
	}
	defer vetx.Close()

	pathToPkg := pkgPaths(root)
	dec := gob.NewDecoder(vetx)
	for {
		var gf gobFact
		err := dec.Decode(&gf)
		if err != nil {
			if err == io.EOF {
				break
			}
			return fmt.Errorf("failed loading cached facts: %w", err)
		}

		pkg, ok := pathToPkg[gf.PkgPath]
		if !ok {
			continue
		}
		if gf.ObjPath == "" {
			pkgFacts[packageFactKey{
				Pkg:  pkg,
				Type: reflect.TypeOf(gf.Fact),
			}] = gf.Fact
		} else {
			obj, err := objectpath.Object(pkg, objectpath.Path(gf.ObjPath))
			if err != nil {
				continue
			}
			objFacts[objectFactKey{
				Obj:  obj,
				Type: reflect.TypeOf(gf.Fact),
			}] = gf.Fact
		}
	}
	return nil
}

func genericHandle(a action, root action, queue chan action, sem *tsync.Semaphore, exec func(a action) error) {
	if a == root {
		close(queue)
		if sem != nil {
			sem.Release()
		}
		return
	}
	if !a.IsFailed() {
		// the action may have already been marked as failed during
		// construction of the action graph, for example because of
		// unresolved imports.

		for _, dep := range a.Deps() {
			if dep.IsFailed() {
				// One of our dependencies failed, so mark this package as
				// failed and bail. We don't need to record an error for
				// this package, the relevant error will have been
				// reported by the first package in the chain that failed.
				a.MarkFailed()
				break
			}
		}
	}

	if !a.IsFailed() {
		if err := exec(a); err != nil {
			a.MarkFailed()
			a.AddError(err)
		}
	}
	if sem != nil {
		sem.Release()
	}

	for _, t := range a.Triggers() {
		if t.DecrementPending() {
			queue <- t
		}
	}
}

type analyzerRunner struct {
	pkg *loader.Package
	// object facts of our dependencies; may contain facts of
	// analyzers other than the current one
	depObjFacts map[objectFactKey]analysis.Fact
	// package facts of our dependencies; may contain facts of
	// analyzers other than the current one
	depPkgFacts map[packageFactKey]analysis.Fact
	factsOnly   bool

	stats *Stats
}

func (ar *analyzerRunner) do(act action) error {
	a := act.(*analyzerAction)
	results := map[*analysis.Analyzer]interface{}{}
	// TODO(dh): does this have to be recursive?
	for _, dep := range a.deps {
		dep := dep.(*analyzerAction)
		results[dep.Analyzer] = dep.Result
	}
	factTypes := map[reflect.Type]struct{}{}
	for _, typ := range a.Analyzer.FactTypes {
		factTypes[reflect.TypeOf(typ)] = struct{}{}
	}
	filterFactType := func(typ reflect.Type) bool {
		_, ok := factTypes[typ]
		return ok
	}
	a.Pass = &analysis.Pass{
		Analyzer:   a.Analyzer,
		Fset:       ar.pkg.Fset,
		Files:      ar.pkg.Syntax,
		OtherFiles: ar.pkg.OtherFiles,
		Pkg:        ar.pkg.Types,
		TypesInfo:  ar.pkg.TypesInfo,
		TypesSizes: ar.pkg.TypesSizes,
		Report: func(d analysis.Diagnostic) {
			if !ar.factsOnly {
				if d.Category == "" {
					d.Category = a.Analyzer.Name
				}
				a.Diagnostics = append(a.Diagnostics, d)
			}
		},
		ResultOf: results,
		ImportObjectFact: func(obj types.Object, fact analysis.Fact) bool {
			key := objectFactKey{
				Obj:  obj,
				Type: reflect.TypeOf(fact),
			}
			if f, ok := ar.depObjFacts[key]; ok {
				reflect.ValueOf(fact).Elem().Set(reflect.ValueOf(f).Elem())
				return true
			} else if f, ok := a.ObjectFacts[key]; ok {
				reflect.ValueOf(fact).Elem().Set(reflect.ValueOf(f).Elem())
				return true
			}
			return false
		},
		ImportPackageFact: func(pkg *types.Package, fact analysis.Fact) bool {
			key := packageFactKey{
				Pkg:  pkg,
				Type: reflect.TypeOf(fact),
			}
			if f, ok := ar.depPkgFacts[key]; ok {
				reflect.ValueOf(fact).Elem().Set(reflect.ValueOf(f).Elem())
				return true
			} else if f, ok := a.PackageFacts[key]; ok {
				reflect.ValueOf(fact).Elem().Set(reflect.ValueOf(f).Elem())
				return true
			}
			return false
		},
		ExportObjectFact: func(obj types.Object, fact analysis.Fact) {
			key := objectFactKey{
				Obj:  obj,
				Type: reflect.TypeOf(fact),
			}
			a.ObjectFacts[key] = fact
		},
		ExportPackageFact: func(fact analysis.Fact) {
			key := packageFactKey{
				Pkg:  ar.pkg.Types,
				Type: reflect.TypeOf(fact),
			}
			a.PackageFacts[key] = fact
		},
		AllPackageFacts: func() []analysis.PackageFact {
			out := make([]analysis.PackageFact, 0, len(ar.depPkgFacts)+len(a.PackageFacts))
			for key, fact := range ar.depPkgFacts {
				out = append(out, analysis.PackageFact{
					Package: key.Pkg,
					Fact:    fact,
				})
			}
			for key, fact := range a.PackageFacts {
				out = append(out, analysis.PackageFact{
					Package: key.Pkg,
					Fact:    fact,
				})
			}
			return out
		},
		AllObjectFacts: func() []analysis.ObjectFact {
			out := make([]analysis.ObjectFact, 0, len(ar.depObjFacts)+len(a.ObjectFacts))
			for key, fact := range ar.depObjFacts {
				if filterFactType(key.Type) {
					out = append(out, analysis.ObjectFact{
						Object: key.Obj,
						Fact:   fact,
					})
				}
			}
			for key, fact := range a.ObjectFacts {
				if filterFactType(key.Type) {
					out = append(out, analysis.ObjectFact{
						Object: key.Obj,
						Fact:   fact,
					})
				}
			}
			return out
		},
	}

	t := time.Now()
	res, err := a.Analyzer.Run(a.Pass)
	ar.stats.measureAnalyzer(a.Analyzer, ar.pkg.PackageSpec, time.Since(t))
	if err != nil {
		return err
	}
	a.Result = res
	return nil
}

type analysisResult struct {
	objFacts    []analysis.ObjectFact
	pkgFacts    []analysis.PackageFact
	diagnostics []analysis.Diagnostic
	unused      unused.SerializedResult
}

func (r *subrunner) runAnalyzers(pkgAct *packageAction, pkg *loader.Package) (analysisResult, error) {
	depObjFacts := map[objectFactKey]analysis.Fact{}
	depPkgFacts := map[packageFactKey]analysis.Fact{}

	for _, dep := range pkgAct.deps {
		if err := r.loadFacts(pkg.Types, dep.(*packageAction), depObjFacts, depPkgFacts); err != nil {
			return analysisResult{}, err
		}
	}

	// OPT(dh): this computation is the same for all packages
	// (depending on act.factsOnly), we should cache it in the runner.
	analyzerActionCache := map[*analysis.Analyzer]*analyzerAction{}
	root := &analyzerAction{}
	for _, a := range r.analyzers {
		// When analyzing non-initial packages, we only care about
		// analyzers that produce facts.
		if !pkgAct.factsOnly || len(a.FactTypes) > 0 {
			a := newAnalyzerAction(a, analyzerActionCache)
			root.deps = append(root.deps, a)
			a.triggers = append(a.triggers, root)
		}
	}
	root.pending = uint32(len(root.deps))
	all := actionList(root)

	ar := &analyzerRunner{
		pkg:         pkg,
		factsOnly:   pkgAct.factsOnly,
		depObjFacts: depObjFacts,
		depPkgFacts: depPkgFacts,
		stats:       &r.Stats,
	}
	queue := make(chan action, len(all))
	for _, a := range all {
		if len(a.Deps()) == 0 {
			queue <- a
		}
	}

	for item := range queue {
		b := r.semaphore.AcquireMaybe()
		if b {
			go genericHandle(item, root, queue, &r.semaphore, ar.do)
		} else {
			// the semaphore is exhausted; run the analysis under the
			// token we've acquired for analyzing the package.
			genericHandle(item, root, queue, nil, ar.do)
		}
	}

	var unusedResult unused.SerializedResult
	for _, a := range all {
		a := a.(*analyzerAction)

		if a != root && a.Analyzer.Name == "U1000" {
			// TODO(dh): figure out a clean abstraction, instead of
			// special-casing U1000.
			unusedResult = unused.Serialize(a.Pass, a.Result.(unused.Result), pkg.Fset)
		}

		for key, fact := range a.ObjectFacts {
			depObjFacts[key] = fact
		}
		for key, fact := range a.PackageFacts {
			depPkgFacts[key] = fact
		}
	}

	// OPT(dh): cull objects not reachable via the exported closure
	objFacts := make([]analysis.ObjectFact, 0, len(depObjFacts))
	pkgFacts := make([]analysis.PackageFact, 0, len(depPkgFacts))
	for key, fact := range depObjFacts {
		objFacts = append(objFacts, analysis.ObjectFact{Object: key.Obj, Fact: fact})
	}
	for key, fact := range depPkgFacts {
		pkgFacts = append(pkgFacts, analysis.PackageFact{Package: key.Pkg, Fact: fact})
	}

	var diags []analysis.Diagnostic
	for _, a := range root.deps {
		a := a.(*analyzerAction)
		diags = append(diags, a.Diagnostics...)
	}
	return analysisResult{
		objFacts:    objFacts,
		pkgFacts:    pkgFacts,
		diagnostics: diags,
		unused:      unusedResult,
	}, nil
}

func actionList(root action) []action {
	seen := map[action]struct{}{}
	all := []action{}
	var walk func(action)
	walk = func(a action) {
		if _, ok := seen[a]; ok {
			return
		}
		seen[a] = struct{}{}
		for _, a1 := range a.Deps() {
			walk(a1)
		}
		all = append(all, a)
	}
	walk(root)
	return all
}

func registerGobTypes(analyzers []*analysis.Analyzer) {
	for _, a := range analyzers {
		for _, typ := range a.FactTypes {
			// FIXME(dh): use RegisterName so we can work around collisions
			// in names. For pointer-types, gob incorrectly qualifies
			// type names with the package name, not the import path.
			gob.Register(typ)
		}
	}
}

func allAnalyzers(analyzers []*analysis.Analyzer) []*analysis.Analyzer {
	seen := map[*analysis.Analyzer]struct{}{}
	out := make([]*analysis.Analyzer, 0, len(analyzers))
	var dfs func(*analysis.Analyzer)
	dfs = func(a *analysis.Analyzer) {
		if _, ok := seen[a]; ok {
			return
		}
		seen[a] = struct{}{}
		out = append(out, a)
		for _, dep := range a.Requires {
			dfs(dep)
		}
	}
	for _, a := range analyzers {
		dfs(a)
	}
	return out
}

// Run loads the packages specified by patterns, runs analyzers on
// them and returns the results. Each result corresponds to a single
// package. Results will be returned for all packages, including
// dependencies. Errors specific to packages will be reported in the
// respective results.
//
// If cfg is nil, a default config will be used. Otherwise, cfg will
// be used, with the exception of the Mode field.
//
// Run can be called multiple times on the same Runner and it is safe
// for concurrent use. All runs will share the same semaphore.
func (r *Runner) Run(cfg *packages.Config, analyzers []*analysis.Analyzer, patterns []string) ([]Result, error) {
	analyzers = allAnalyzers(analyzers)
	registerGobTypes(analyzers)

	for _, a := range analyzers {
		flag := a.Flags.Lookup("go")
		if flag == nil {
			continue
		}
		// OPT(dh): this is terrible
		flag.Value.Set(fmt.Sprintf("1.%d", r.GoVersion))
	}

	r.Stats.setState(StateLoadPackageGraph)
	lpkgs, err := loader.Graph(cfg, patterns...)
	if err != nil {
		return nil, err
	}
	r.Stats.setInitialPackages(len(lpkgs))

	r.Stats.setState(StateBuildActionGraph)
	packageActionCache := map[*loader.PackageSpec]*packageAction{}
	root := &packageAction{}
	for _, lpkg := range lpkgs {
		a := newPackageActionRoot(lpkg, packageActionCache)
		root.deps = append(root.deps, a)
		a.triggers = append(a.triggers, root)
	}
	root.pending = uint32(len(root.deps))

	all := actionList(root)
	queue := make(chan action)
	r.Stats.setTotalPackages(len(all) - 1)

	r.Stats.setState(StateProcessing)
	go func() {
		for _, a := range all {
			if len(a.Deps()) == 0 {
				queue <- a
			}
		}
	}()

	sr := newSubrunner(r, analyzers)
	for item := range queue {
		r.semaphore.Acquire()
		go genericHandle(item, root, queue, &r.semaphore, func(act action) error {
			return sr.do(act)
		})
	}

	r.Stats.setState(StateFinalizing)
	out := make([]Result, len(all)-1)
	for i, item := range all {
		item := item.(*packageAction)
		if item.Package == nil {
			continue
		}
		out[i] = Result{
			Package:     item.Package,
			Config:      item.cfg,
			Failed:      item.failed,
			Errors:      item.errors,
			diagnostics: item.diagnostics,
			directives:  item.directives,
			unused:      item.unused,
		}
	}
	return out, nil
}
