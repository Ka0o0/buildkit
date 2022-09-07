package llbsolver

import (
	"context"
	"encoding/base64"
	"fmt"
	"strings"
	"time"

	"github.com/moby/buildkit/cache"
	cacheconfig "github.com/moby/buildkit/cache/config"
	"github.com/moby/buildkit/cache/remotecache"
	"github.com/moby/buildkit/client"
	controlgateway "github.com/moby/buildkit/control/gateway"
	"github.com/moby/buildkit/exporter"
	"github.com/moby/buildkit/exporter/containerimage/exptypes"
	"github.com/moby/buildkit/frontend"
	"github.com/moby/buildkit/frontend/gateway"
	"github.com/moby/buildkit/identity"
	"github.com/moby/buildkit/session"
	"github.com/moby/buildkit/solver"
	"github.com/moby/buildkit/solver/llbsolver/provenance"
	"github.com/moby/buildkit/solver/result"
	"github.com/moby/buildkit/util/buildinfo"
	"github.com/moby/buildkit/util/compression"
	"github.com/moby/buildkit/util/entitlements"
	"github.com/moby/buildkit/util/progress"
	"github.com/moby/buildkit/worker"
	digest "github.com/opencontainers/go-digest"
	"github.com/pkg/errors"
	"golang.org/x/sync/errgroup"
)

const keyEntitlements = "llb.entitlements"

type ExporterRequest struct {
	Exporter       exporter.ExporterInstance
	CacheExporters []RemoteCacheExporter
}

type RemoteCacheExporter struct {
	remotecache.Exporter
	solver.CacheExportMode
}

// ResolveWorkerFunc returns default worker for the temporary default non-distributed use cases
type ResolveWorkerFunc func() (worker.Worker, error)

// Opt defines options for new Solver.
type Opt struct {
	CacheManager     solver.CacheManager
	CacheResolvers   map[string]remotecache.ResolveCacheImporterFunc
	Entitlements     []string
	Frontends        map[string]frontend.Frontend
	GatewayForwarder *controlgateway.GatewayForwarder
	SessionManager   *session.Manager
	WorkerController *worker.Controller
}

type Solver struct {
	workerController          *worker.Controller
	solver                    *solver.Solver
	resolveWorker             ResolveWorkerFunc
	eachWorker                func(func(worker.Worker) error) error
	frontends                 map[string]frontend.Frontend
	resolveCacheImporterFuncs map[string]remotecache.ResolveCacheImporterFunc
	gatewayForwarder          *controlgateway.GatewayForwarder
	sm                        *session.Manager
	entitlements              []string
}

// Processor defines a processing function to be applied after solving, but
// before exporting
type Processor func(ctx context.Context, result *Result, s *Solver, j *solver.Job) (*Result, error)

func New(opt Opt) (*Solver, error) {
	s := &Solver{
		workerController:          opt.WorkerController,
		resolveWorker:             defaultResolver(opt.WorkerController),
		eachWorker:                allWorkers(opt.WorkerController),
		frontends:                 opt.Frontends,
		resolveCacheImporterFuncs: opt.CacheResolvers,
		gatewayForwarder:          opt.GatewayForwarder,
		sm:                        opt.SessionManager,
		entitlements:              opt.Entitlements,
	}

	s.solver = solver.NewSolver(solver.SolverOpt{
		ResolveOpFunc: s.resolver(),
		DefaultCache:  opt.CacheManager,
	})
	return s, nil
}

func (s *Solver) resolver() solver.ResolveOpFunc {
	return func(v solver.Vertex, b solver.Builder) (solver.Op, error) {
		w, err := s.resolveWorker()
		if err != nil {
			return nil, err
		}
		return w.ResolveOp(v, s.Bridge(b), s.sm)
	}
}

func (s *Solver) bridge(b solver.Builder) *provenanceBridge {
	return &provenanceBridge{llbBridge: &llbBridge{
		builder:                   b,
		frontends:                 s.frontends,
		resolveWorker:             s.resolveWorker,
		eachWorker:                s.eachWorker,
		resolveCacheImporterFuncs: s.resolveCacheImporterFuncs,
		cms:                       map[string]solver.CacheManager{},
		sm:                        s.sm,
	}}
}

func (s *Solver) Bridge(b solver.Builder) frontend.FrontendLLBBridge {
	return s.bridge(b)
}

func (s *Solver) Solve(ctx context.Context, id string, sessionID string, req frontend.SolveRequest, exp ExporterRequest, ent []entitlements.Entitlement, post []Processor) (*client.SolveResponse, error) {
	j, err := s.solver.NewJob(id)
	if err != nil {
		return nil, err
	}

	defer j.Discard()

	set, err := entitlements.WhiteList(ent, supportedEntitlements(s.entitlements))
	if err != nil {
		return nil, err
	}
	j.SetValue(keyEntitlements, set)

	j.SessionID = sessionID

	var res *frontend.Result
	br := s.bridge(j)
	if s.gatewayForwarder != nil && req.Definition == nil && req.Frontend == "" {
		fwd := gateway.NewBridgeForwarder(ctx, br, s.workerController, req.FrontendInputs, sessionID, s.sm)
		defer fwd.Discard()
		if err := s.gatewayForwarder.RegisterBuild(ctx, id, fwd); err != nil {
			return nil, err
		}
		defer s.gatewayForwarder.UnregisterBuild(ctx, id)

		var err error
		select {
		case <-fwd.Done():
			res, err = fwd.Result()
		case <-ctx.Done():
			err = ctx.Err()
		}
		if err != nil {
			return nil, err
		}
	} else {
		res, err = br.Solve(ctx, req, sessionID)
		if err != nil {
			return nil, err
		}
	}

	if res == nil {
		res = &frontend.Result{}
	}

	defer func() {
		res.EachRef(func(ref solver.ResultProxy) error {
			go ref.Release(context.TODO())
			return nil
		})
	}()

	eg, ctx2 := errgroup.WithContext(ctx)
	res.EachRef(func(ref solver.ResultProxy) error {
		eg.Go(func() error {
			_, err := ref.Result(ctx2)
			return err
		})
		return nil
	})
	if err := eg.Wait(); err != nil {
		return nil, err
	}

	resProv, err := addProvenanceToResult(res, br)
	if err != nil {
		return nil, err
	}

	for _, post := range post {
		res2, err := post(ctx, resProv, s, j)
		if err != nil {
			return nil, err
		}
		resProv = res2
	}
	res = resProv.Result

	cached, err := result.ConvertResult(res, func(res solver.ResultProxy) (solver.CachedResult, error) {
		return res.Result(ctx)
	})
	if err != nil {
		return nil, err
	}
	inp, err := result.ConvertResult(cached, func(res solver.CachedResult) (cache.ImmutableRef, error) {
		workerRef, ok := res.Sys().(*worker.WorkerRef)
		if !ok {
			return nil, errors.Errorf("invalid reference: %T", res.Sys())
		}
		return workerRef.ImmutableRef, nil
	})
	if err != nil {
		return nil, err
	}

	var exporterResponse map[string]string
	cacheExporters, inlineCacheExporter := splitCacheExporters(exp.CacheExporters)
	if e := exp.Exporter; e != nil {
		exporterResponse, err = exportWithInlineCacheExporter(ctx, e, inlineCacheExporter, j, cached, inp)
		if err != nil {
			return nil, err
		}
	}

	cacheExporterResponse, err := runCacheExporters(ctx, cacheExporters, j, cached, inp)
	if err != nil {
		return nil, err
	}

	if exporterResponse == nil {
		exporterResponse = make(map[string]string)
	}

	for k, v := range res.Metadata {
		if strings.HasPrefix(k, "frontend.") {
			exporterResponse[k] = string(v)
		}
		if strings.HasPrefix(k, exptypes.ExporterBuildInfo) {
			exporterResponse[k] = base64.StdEncoding.EncodeToString(v)
		}
	}
	for k, v := range cacheExporterResponse {
		if strings.HasPrefix(k, "cache.") {
			exporterResponse[k] = v
		}
	}

	return &client.SolveResponse{
		ExporterResponse: exporterResponse,
	}, nil
}

func runCacheExporters(ctx context.Context, exporters []RemoteCacheExporter, j *solver.Job, cached *result.Result[solver.CachedResult], inp *result.Result[cache.ImmutableRef]) (map[string]string, error) {
	eg, ctx := errgroup.WithContext(ctx)
	g := session.NewGroup(j.SessionID)
	var cacheExporterResponse map[string]string
	resps := make([]map[string]string, len(exporters))
	for i, exp := range exporters {
		func(exp RemoteCacheExporter, i int) {
			eg.Go(func() (err error) {
				id := fmt.Sprint(j.SessionID, "-cache-", i)
				return inBuilderContext(ctx, j, fmt.Sprint("exporting ", exp.Exporter.Name()), id, func(ctx context.Context, _ session.Group) error {
					prepareDone := progress.OneOff(ctx, "preparing build cache for export")
					if err := result.EachRef(cached, inp, func(res solver.CachedResult, ref cache.ImmutableRef) error {
						ctx = withDescHandlerCacheOpts(ctx, ref)

						// Configure compression
						compressionConfig := exp.Config().Compression

						// all keys have same export chain so exporting others is not needed
						_, err = res.CacheKeys()[0].Exporter.ExportTo(ctx, exp, solver.CacheExportOpt{
							ResolveRemotes: workerRefResolver(cacheconfig.RefConfig{Compression: compressionConfig}, false, g),
							Mode:           exp.CacheExportMode,
							Session:        g,
							CompressionOpt: &compressionConfig,
						})
						return err
					}); err != nil {
						return prepareDone(err)
					}
					prepareDone(nil)
					resps[i], err = exp.Finalize(ctx)
					if err != nil {
						return err
					}
					return nil
				})
			})
		}(exp, i)
	}
	if err := eg.Wait(); err != nil {
		return nil, err
	}
	for _, resp := range resps {
		for k, v := range resp {
			if cacheExporterResponse == nil {
				cacheExporterResponse = make(map[string]string)
			}
			cacheExporterResponse[k] = v
		}
	}
	return cacheExporterResponse, nil
}

func exportWithInlineCacheExporter(ctx context.Context, e exporter.ExporterInstance, inlineExporter *RemoteCacheExporter, j *solver.Job, cached *result.Result[solver.CachedResult], inp *result.Result[cache.ImmutableRef]) (exporterResponse map[string]string, err error) {
	if inlineExporter != nil {
		if err := inBuilderContext(ctx, j, "preparing layers for inline cache", j.SessionID+"-cache-inline", func(ctx context.Context, _ session.Group) error {
			if cached.Ref != nil {
				dtic, err := inlineCache(ctx, inlineExporter.Exporter, cached.Ref, e.Config().Compression(), session.NewGroup(j.SessionID))
				if err != nil {
					return err
				}
				if dtic != nil {
					inp.AddMeta(exptypes.ExporterInlineCache, dtic)
				}
			}
			for k, res := range cached.Refs {
				dtic, err := inlineCache(ctx, inlineExporter.Exporter, res, e.Config().Compression(), session.NewGroup(j.SessionID))
				if err != nil {
					return err
				}
				if dtic != nil {
					inp.AddMeta(fmt.Sprintf("%s/%s", exptypes.ExporterInlineCache, k), dtic)
				}
			}
			return nil
		}); err != nil {
			return nil, err
		}
	}
	if err := inBuilderContext(ctx, j, e.Name(), j.SessionID+"-export", func(ctx context.Context, _ session.Group) error {
		exporterResponse, err = e.Export(ctx, inp, j.SessionID)
		return err
	}); err != nil {
		return nil, err
	}
	return exporterResponse, nil
}

func splitCacheExporters(exporters []RemoteCacheExporter) (rest []RemoteCacheExporter, inline *RemoteCacheExporter) {
	rest = make([]RemoteCacheExporter, 0, len(exporters))
	for i, exp := range exporters {
		if _, ok := asInlineCache(exp.Exporter); ok {
			inline = &exporters[i]
			continue
		}
		rest = append(rest, exp)
	}
	return rest, inline
}

func addProvenanceToResult(res *frontend.Result, br *provenanceBridge) (*Result, error) {
	if res == nil {
		return nil, nil
	}
	reqs, err := br.requests(res)
	if err != nil {
		return nil, err
	}
	out := &Result{
		Result:     res,
		Provenance: &provenance.Result{},
	}
	if res.Ref != nil {
		cp, err := getProvenance(res.Ref, reqs.ref.bridge, "", reqs)
		if err != nil {
			return nil, err
		}
		out.Provenance.Ref = cp
		if res.Metadata == nil {
			res.Metadata = map[string][]byte{}
		}
		if err := buildinfo.AddMetadata(res.Metadata, exptypes.ExporterBuildInfo, cp); err != nil {
			return nil, err
		}
	}
	if len(res.Refs) == 0 {
		return out, nil
	}
	out.Provenance.Refs = make(map[string]*provenance.Capture, len(res.Refs))

	for k, ref := range res.Refs {
		cp, err := getProvenance(ref, reqs.refs[k].bridge, k, reqs)
		if err != nil {
			return nil, err
		}
		out.Provenance.Refs[k] = cp
		if res.Metadata == nil {
			res.Metadata = map[string][]byte{}
		}
		if err := buildinfo.AddMetadata(res.Metadata, fmt.Sprintf("%s/%s", exptypes.ExporterBuildInfo, k), cp); err != nil {
			return nil, err
		}
	}
	return out, nil
}

func getRefProvenance(ref solver.ResultProxy, br *provenanceBridge) (*provenance.Capture, error) {
	if ref == nil {
		return nil, nil
	}
	p := ref.Provenance()
	if p == nil {
		return nil, errors.Errorf("missing provenance for %s", ref.ID())
	}
	pr, ok := p.(*provenance.Capture)
	if !ok {
		return nil, errors.Errorf("invalid provenance type %T", p)
	}

	if br.req != nil {
		pr.Frontend = br.req.Frontend
		pr.Args = provenance.FilterArgs(br.req.FrontendOpt)
		// TODO: should also save some output options like compression

		if len(br.req.FrontendInputs) > 0 {
			pr.IncompleteMaterials = true // not implemented
		}
	}

	return pr, nil
}

func getProvenance(ref solver.ResultProxy, br *provenanceBridge, id string, reqs *resultRequests) (*provenance.Capture, error) {
	pr, err := getRefProvenance(ref, br)
	if err != nil {
		return nil, err
	}
	if pr == nil {
		return nil, nil
	}

	visited := reqs.allRes()
	visited[ref.ID()] = struct{}{}
	// provenance for all the refs not directly in the result needs to be captured as well
	if err := br.eachRef(func(r solver.ResultProxy) error {
		if _, ok := visited[r.ID()]; ok {
			return nil
		}
		visited[r.ID()] = struct{}{}
		pr2, err := getRefProvenance(r, br)
		if err != nil {
			return err
		}
		return pr.Merge(pr2)
	}); err != nil {
		return nil, err
	}

	imgs := br.allImages()
	if id != "" {
		imgs = reqs.filterImagePlatforms(id, imgs)
	}
	for _, img := range imgs {
		pr.AddImage(img)
	}

	if err := pr.OptimizeImageSources(); err != nil {
		return nil, err
	}
	pr.Sort()

	return pr, nil
}

type inlineCacheExporter interface {
	ExportForLayers(context.Context, []digest.Digest) ([]byte, error)
}

func asInlineCache(e remotecache.Exporter) (inlineCacheExporter, bool) {
	ie, ok := e.(inlineCacheExporter)
	return ie, ok
}

func inlineCache(ctx context.Context, e remotecache.Exporter, res solver.CachedResult, compressionopt compression.Config, g session.Group) ([]byte, error) {
	ie, ok := asInlineCache(e)
	if !ok {
		return nil, nil
	}
	workerRef, ok := res.Sys().(*worker.WorkerRef)
	if !ok {
		return nil, errors.Errorf("invalid reference: %T", res.Sys())
	}

	remotes, err := workerRef.GetRemotes(ctx, true, cacheconfig.RefConfig{Compression: compressionopt}, false, g)
	if err != nil || len(remotes) == 0 {
		return nil, nil
	}
	remote := remotes[0]

	digests := make([]digest.Digest, 0, len(remote.Descriptors))
	for _, desc := range remote.Descriptors {
		digests = append(digests, desc.Digest)
	}

	ctx = withDescHandlerCacheOpts(ctx, workerRef.ImmutableRef)
	refCfg := cacheconfig.RefConfig{Compression: compressionopt}
	if _, err := res.CacheKeys()[0].Exporter.ExportTo(ctx, e, solver.CacheExportOpt{
		ResolveRemotes: workerRefResolver(refCfg, true, g), // load as many compression blobs as possible
		Mode:           solver.CacheExportModeMin,
		Session:        g,
		CompressionOpt: &compressionopt, // cache possible compression variants
	}); err != nil {
		return nil, err
	}
	return ie.ExportForLayers(ctx, digests)
}

func withDescHandlerCacheOpts(ctx context.Context, ref cache.ImmutableRef) context.Context {
	return solver.WithCacheOptGetter(ctx, func(includeAncestors bool, keys ...interface{}) map[interface{}]interface{} {
		vals := make(map[interface{}]interface{})
		for _, k := range keys {
			if key, ok := k.(cache.DescHandlerKey); ok {
				if handler := ref.DescHandler(digest.Digest(key)); handler != nil {
					vals[k] = handler
				}
			}
		}
		return vals
	})
}

func (s *Solver) Status(ctx context.Context, id string, statusChan chan *client.SolveStatus) error {
	j, err := s.solver.Get(id)
	if err != nil {
		close(statusChan)
		return err
	}
	return j.Status(ctx, statusChan)
}

func defaultResolver(wc *worker.Controller) ResolveWorkerFunc {
	return func() (worker.Worker, error) {
		return wc.GetDefault()
	}
}
func allWorkers(wc *worker.Controller) func(func(w worker.Worker) error) error {
	return func(f func(worker.Worker) error) error {
		all, err := wc.List()
		if err != nil {
			return err
		}
		for _, w := range all {
			if err := f(w); err != nil {
				return err
			}
		}
		return nil
	}
}

func inBuilderContext(ctx context.Context, b solver.Builder, name, id string, f func(ctx context.Context, g session.Group) error) error {
	if id == "" {
		id = name
	}
	v := client.Vertex{
		Digest: digest.FromBytes([]byte(id)),
		Name:   name,
	}
	return b.InContext(ctx, func(ctx context.Context, g session.Group) error {
		pw, _, ctx := progress.NewFromContext(ctx, progress.WithMetadata("vertex", v.Digest))
		notifyCompleted := notifyStarted(ctx, &v, false)
		defer pw.Close()
		err := f(ctx, g)
		notifyCompleted(err, false)
		return err
	})
}

func notifyStarted(ctx context.Context, v *client.Vertex, cached bool) func(err error, cached bool) {
	pw, _, _ := progress.NewFromContext(ctx)
	start := time.Now()
	v.Started = &start
	v.Completed = nil
	v.Cached = cached
	id := identity.NewID()
	pw.Write(id, *v)
	return func(err error, cached bool) {
		defer pw.Close()
		stop := time.Now()
		v.Completed = &stop
		v.Cached = cached
		if err != nil {
			v.Error = err.Error()
		}
		pw.Write(id, *v)
	}
}

func supportedEntitlements(ents []string) []entitlements.Entitlement {
	out := []entitlements.Entitlement{} // nil means no filter
	for _, e := range ents {
		if e == string(entitlements.EntitlementNetworkHost) {
			out = append(out, entitlements.EntitlementNetworkHost)
		}
		if e == string(entitlements.EntitlementSecurityInsecure) {
			out = append(out, entitlements.EntitlementSecurityInsecure)
		}
	}
	return out
}

func loadEntitlements(b solver.Builder) (entitlements.Set, error) {
	var ent entitlements.Set = map[entitlements.Entitlement]struct{}{}
	err := b.EachValue(context.TODO(), keyEntitlements, func(v interface{}) error {
		set, ok := v.(entitlements.Set)
		if !ok {
			return errors.Errorf("invalid entitlements %T", v)
		}
		for k := range set {
			ent[k] = struct{}{}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return ent, nil
}
