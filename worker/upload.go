package worker

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"sort"
	"sync"
	"time"

	"github.com/montanaflynn/stats"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
	rhpv2 "go.sia.tech/core/rhp/v2"
	"go.sia.tech/core/types"
	"go.sia.tech/mux/v1"
	"go.sia.tech/renterd/api"
	"go.sia.tech/renterd/object"
	"go.sia.tech/renterd/tracing"
	"lukechampine.com/frand"
)

var (
	errNoFreeUploader     = errors.New("no free uploader")
	errNotEnoughUploaders = errors.New("not enough uploaders to support requested redundancy")
)

type (
	uploadID [8]byte
	slabID   [8]byte

	uploadManager struct {
		hp hostProvider
		rl revisionLocker

		maxOverdrive     uint64
		overdriveTimeout time.Duration

		statsOverdrive *dataPoints
		statsSpeed     *dataPoints
		stopChan       chan struct{}

		mu        sync.Mutex
		uploaders []*uploader
	}

	uploader struct {
		fcid       types.FileContractID
		hk         types.PublicKey
		siamuxAddr string

		mu                  sync.Mutex
		bh                  uint64
		consecutiveFailures uint64
		signalNewUpload     chan struct{}
		queue               []*shardUpload

		statsSpeed *dataPoints
		stopChan   chan struct{}
	}

	upload struct {
		mgr *uploadManager

		uID uploadID

		excluded         map[types.FileContractID]struct{}
		nextReadTrigger  chan struct{}
		doneShardTrigger chan struct{}

		mu      sync.Mutex
		ongoing []slabID
		used    map[slabID]map[types.FileContractID]struct{}
	}

	slabUpload struct {
		mgr *uploadManager

		upload  *upload
		sID     slabID
		started time.Time

		mu          sync.Mutex
		numInflight uint64
		numLaunched uint64

		nextReadTriggered bool
		lastOverdrive     time.Time
		overdriving       map[int]int
		remaining         map[int]shardCtx
		sectors           []object.Sector
		errs              HostErrorSet
	}

	slabResponse struct {
		slab  object.SlabSlice
		index int
		err   error
	}

	shardCtx struct {
		ctx    context.Context
		cancel context.CancelFunc
	}

	shardUpload struct {
		upload *upload
		sID    slabID
		ctx    context.Context

		overdrive    bool
		responseChan chan shardResp

		sector      *[rhpv2.SectorSize]byte
		sectorIndex int

		// these fields are set by the uploader when the upload is scheduled
		fcid       types.FileContractID
		hk         types.PublicKey
		siamuxAddr string
	}

	shardResp struct {
		req  *shardUpload
		root types.Hash256
		err  error
	}

	uploadManagerStats struct {
		avgUploadSpeedMBPS  float64
		healthyUploaders    uint64
		numUploaders        uint64
		overdrivePct        float64
		uploadSpeedsP90MBPS map[types.PublicKey]float64
	}

	dataPoints struct {
		stats.Float64Data

		mu  sync.Mutex
		cnt int
		p90 float64
	}
)

func (w *worker) initUploadManager() {
	if w.uploadManager != nil {
		panic("uploader already initialized") // developer error
	}

	w.uploadManager = newUploadManager(w, w, w.uploadMaxOverdrive, w.uploadOverdriveTimeout)
}

func newDataPoints() *dataPoints {
	return &dataPoints{
		Float64Data: make([]float64, 20),
	}
}

func newUploadManager(hp hostProvider, rl revisionLocker, maxOverdrive uint64, sectorTimeout time.Duration) *uploadManager {
	return &uploadManager{
		hp: hp,
		rl: rl,

		maxOverdrive:     maxOverdrive,
		overdriveTimeout: sectorTimeout,

		statsOverdrive: newDataPoints(),
		statsSpeed:     newDataPoints(),

		stopChan: make(chan struct{}),

		uploaders: make([]*uploader, 0),
	}
}

func (mgr *uploadManager) Migrate(ctx context.Context, shards [][]byte, excluded map[types.FileContractID]struct{}) ([]object.Sector, error) {
	// initiate the upload
	upload, err := mgr.newUpload(len(shards), excluded)
	if err != nil {
		return nil, err
	}

	// upload the shards
	return upload.uploadShards(ctx, shards, 0)
}

func (mgr *uploadManager) RefreshUploaders(contracts []api.ContractMetadata, bh uint64) {
	mgr.mu.Lock()
	defer mgr.mu.Unlock()

	// build map
	c2m := make(map[types.FileContractID]api.ContractMetadata)
	for _, c := range contracts {
		c2m[c.ID] = c
	}

	// recreate the pool
	var i int
	for _, q := range mgr.uploaders {
		if _, keep := c2m[q.fcid]; !keep {
			continue
		}
		delete(c2m, q.fcid)
		mgr.uploaders[i] = q
		i++
	}
	for j := i; j < len(mgr.uploaders); j++ {
		mgr.uploaders[j] = nil
	}
	mgr.uploaders = mgr.uploaders[:i]

	// add missing uploaders
	for _, contract := range c2m {
		uploader := mgr.newUploader(contract)
		mgr.uploaders = append(mgr.uploaders, uploader)
		go uploader.start(mgr.hp, mgr.rl)
	}

	// update blockheight
	for _, u := range mgr.uploaders {
		u.updateBlockHeight(bh)
	}
}

func (mgr *uploadManager) Stats() uploadManagerStats {
	mgr.mu.Lock()
	defer mgr.mu.Unlock()

	// prepare stats
	stats := uploadManagerStats{
		avgUploadSpeedMBPS:  mgr.statsSpeed.recompute() * 0.008, // convert bytes per ms to mbps,
		overdrivePct:        mgr.statsOverdrive.recompute(),
		numUploaders:        uint64(len(mgr.uploaders)),
		uploadSpeedsP90MBPS: make(map[types.PublicKey]float64),
	}

	// fill in uploader stats
	for _, u := range mgr.uploaders {
		u.statsSpeed.recompute()
		stats.uploadSpeedsP90MBPS[u.hk] = u.statsSpeed.percentileP90() * 0.008 // convert bytes per ms to mbps
		if u.healthy() {
			stats.healthyUploaders++
		}
	}

	return stats
}

func (mgr *uploadManager) Stop() {
	mgr.mu.Lock()
	defer mgr.mu.Unlock()
	close(mgr.stopChan)
	for _, u := range mgr.uploaders {
		close(u.stopChan)
	}
}

func (mgr *uploadManager) Upload(ctx context.Context, r io.Reader, rs api.RedundancySettings) (_ object.Object, err error) {
	// add cancel
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	// add tracing
	ctx, span := tracing.Tracer.Start(ctx, "upload")
	defer func() {
		span.RecordError(err)
		span.End()
	}()

	// create the object
	o := object.NewObject()

	// create the cipher reader
	cr := o.Encrypt(r)

	// create the upload
	u, err := mgr.newUpload(rs.TotalShards, nil)
	if err != nil {
		return object.Object{}, err
	}

	// create the response channel
	respChan := make(chan slabResponse)
	defer close(respChan)

	// collect the responses
	var responses []slabResponse
	slabIndex := 0
	numSlabs := -1

	// prepare slab size
	size := int64(rs.MinShards) * rhpv2.SectorSize
loop:
	for {
		select {
		case <-u.mgr.stopChan:
			return object.Object{}, errors.New("manager was stopped")
		case <-ctx.Done():
			return object.Object{}, errors.New("upload timed out")
		case <-u.nextReadTrigger:
			// read next slab's data
			data := make([]byte, size)
			length, err := io.ReadFull(io.LimitReader(cr, size), data)
			if err == io.EOF {
				numSlabs = slabIndex
				continue
			} else if err != nil && err != io.ErrUnexpectedEOF {
				return object.Object{}, err
			}

			go u.uploadSlab(ctx, rs, data, length, slabIndex, respChan)
			slabIndex++
		case res := <-respChan:
			if res.err != nil {
				return object.Object{}, res.err
			}
			responses = append(responses, res)
			if len(responses) == numSlabs {
				break loop
			}
		}
	}

	// sort the slabs by index
	sort.Slice(responses, func(i, j int) bool {
		return responses[i].index < responses[j].index
	})

	// decorate the object with the slabs
	for _, resp := range responses {
		o.Slabs = append(o.Slabs, resp.slab)
	}
	return o, nil
}

func (mgr *uploadManager) enqueue(s *shardUpload) error {
	uploader := mgr.uploader(s)
	if uploader == nil {
		return errNoFreeUploader
	}
	uploader.schedule(s)

	mgr.mu.Lock()
	defer mgr.mu.Unlock()
	s.upload.registerUsedUploader(s.sID, uploader.fcid)
	return nil
}

func (mgr *uploadManager) newUpload(totalShards int, excluded map[types.FileContractID]struct{}) (*upload, error) {
	mgr.mu.Lock()
	defer mgr.mu.Unlock()

	// check if we have enough uploaders
	var usable int
	for _, u := range mgr.uploaders {
		if _, exclude := excluded[u.fcid]; exclude {
			continue
		}
		usable++
	}
	if usable < totalShards {
		return nil, errNotEnoughUploaders
	}

	// create id
	var id uploadID
	frand.Read(id[:])

	// create upload
	upload := &upload{
		mgr: mgr,
		uID: id,

		excluded:         excluded,
		nextReadTrigger:  make(chan struct{}, 1),
		doneShardTrigger: make(chan struct{}, 1),

		ongoing: make([]slabID, 0),
		used:    make(map[slabID]map[types.FileContractID]struct{}),
	}
	upload.nextReadTrigger <- struct{}{} // trigger first read
	return upload, nil
}

func (mgr *uploadManager) newUploader(c api.ContractMetadata) *uploader {
	return &uploader{
		fcid:       c.ID,
		hk:         c.HostKey,
		siamuxAddr: c.SiamuxAddr,

		queue:           make([]*shardUpload, 0),
		signalNewUpload: make(chan struct{}, 1),

		statsSpeed: newDataPoints(),
		stopChan:   make(chan struct{}),
	}
}

func (mgr *uploadManager) numUploaders() int {
	mgr.mu.Lock()
	defer mgr.mu.Unlock()
	return len(mgr.uploaders)
}

func (mgr *uploadManager) uploader(shard *shardUpload) *uploader {
	mgr.mu.Lock()
	if len(mgr.uploaders) == 0 {
		mgr.mu.Unlock()
		return nil
	}

	// grab the upload
	upload := shard.upload

	// recompute the stats first
	for _, uploader := range mgr.uploaders {
		uploader.statsSpeed.recompute()
	}

	// sort the uploaders by their estimate
	sort.Slice(mgr.uploaders, func(i, j int) bool {
		return mgr.uploaders[i].estimate() < mgr.uploaders[j].estimate()
	})

	// filter queues
	var candidates []*uploader
	for _, uploader := range mgr.uploaders {
		if upload.canUseUploader(uploader, shard.sID) {
			candidates = append(candidates, uploader)
		}
	}
	mgr.mu.Unlock()

	// return early if we have no queues left
	if len(candidates) == 0 {
		return nil
	}

loop:
	for {
		// grab ongoing slab uploads
		upload.mu.Lock()
		ongoing := upload.ongoing
		upload.mu.Unlock()

		// grab the slabs parents
		var parents []slabID
		for _, sID := range ongoing {
			if sID == shard.sID {
				break
			}
			parents = append(parents, sID)
		}

		// if this slab does not have more than 2 parents, we return the first
		// (and thus best) candidate
		if len(parents) < 3 {
			return candidates[0]
		}

		// otherwise we wait, allowing the parents to complete
		select {
		case <-upload.doneShardTrigger:
			continue loop
		case <-shard.ctx.Done():
			break loop
		}
	}

	return nil
}

func (u *upload) finishSlabUpload(upload *slabUpload) {
	// update ongoing slab history
	u.mu.Lock()
	for i, prev := range u.ongoing {
		if prev == upload.sID {
			u.ongoing = append(u.ongoing[:i], u.ongoing[i+1:]...)
			break
		}
	}
	u.mu.Unlock()

	// cleanup contexts
	upload.mu.Lock()
	for _, shard := range upload.remaining {
		shard.cancel()
	}
	upload.mu.Unlock()
}

func (u *upload) newSlabUpload(ctx context.Context, shards [][]byte) (*slabUpload, []*shardUpload, chan shardResp) {
	u.mu.Lock()
	defer u.mu.Unlock()

	// create slab id
	var sID slabID
	frand.Read(sID[:])
	u.ongoing = append(u.ongoing, sID)

	// create slab upload
	slab := &slabUpload{
		mgr: u.mgr,

		upload:  u,
		sID:     sID,
		started: time.Now(),

		overdriving: make(map[int]int, len(shards)),
		remaining:   make(map[int]shardCtx, len(shards)),
		sectors:     make([]object.Sector, len(shards)),
	}

	// prepare shard uploads
	responseChan := make(chan shardResp)
	uploads := make([]*shardUpload, len(shards))
	for sI, shard := range shards {
		// create the shard upload's cancel func
		sCtx, cancel := context.WithCancel(ctx)
		slab.remaining[sI] = shardCtx{ctx: sCtx, cancel: cancel}

		// create the upload's span
		sCtx, span := tracing.Tracer.Start(sCtx, "uploadShard")
		span.SetAttributes(attribute.Bool("overdrive", false))
		span.SetAttributes(attribute.Int("sector", sI))

		// create the shard upload
		uploads[sI] = &shardUpload{
			upload:       u,
			sID:          sID,
			ctx:          sCtx,
			responseChan: responseChan,

			sector:      (*[rhpv2.SectorSize]byte)(shard),
			sectorIndex: sI,
		}
	}

	return slab, uploads, responseChan
}

func (u *upload) canUseUploader(ul *uploader, sID slabID) bool {
	u.mu.Lock()
	defer u.mu.Unlock()

	_, excluded := u.excluded[ul.fcid]
	if excluded {
		return false
	}

	_, used := u.used[sID][ul.fcid]
	return !used
}

func (u *upload) uploadSlab(ctx context.Context, rs api.RedundancySettings, data []byte, length, index int, respChan chan slabResponse) {
	// add tracing
	ctx, span := tracing.Tracer.Start(ctx, "uploadSlab")
	defer span.End()

	// create the response
	resp := slabResponse{
		slab: object.SlabSlice{
			Slab:   object.NewSlab(uint8(rs.MinShards)),
			Offset: 0,
			Length: uint32(length),
		},
		index: index,
	}

	// create the shards
	shards := make([][]byte, rs.TotalShards)
	resp.slab.Slab.Encode(data, shards)
	resp.slab.Slab.Encrypt(shards)

	// upload the shards
	resp.slab.Slab.Shards, resp.err = u.uploadShards(ctx, shards, index)

	// send the response
	select {
	case <-ctx.Done():
	case respChan <- resp:
	}
}

func (u *upload) registerUsedUploader(sID slabID, fcid types.FileContractID) {
	u.mu.Lock()
	defer u.mu.Unlock()

	_, exists := u.used[sID]
	if !exists {
		u.used[sID] = make(map[types.FileContractID]struct{})
	}
	u.used[sID][fcid] = struct{}{}
}

func (u *upload) uploadShards(ctx context.Context, shards [][]byte, index int) ([]object.Sector, error) {
	ctx, span := tracing.Tracer.Start(ctx, "uploadShards")
	defer span.End()

	// convenience variables
	mgr := u.mgr

	// prepare the upload
	slab, uploads, shardRespChan := u.newSlabUpload(ctx, shards)
	span.SetAttributes(attribute.Stringer("id", slab.sID))
	defer u.finishSlabUpload(slab)

	// launch all shard uploads
	for _, upload := range uploads {
		if err := slab.launch(upload); err != nil {
			return nil, err
		}
	}

	// create a timer to trigger overdrive
	timeout := time.NewTimer(mgr.overdriveTimeout)
	resetTimeout := func() {
		timeout.Stop()
		select {
		case <-timeout.C:
		default:
		}
		timeout.Reset(mgr.overdriveTimeout)
	}

	// launch a goroutine to trigger overdrive
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case <-timeout.C:
				if upload := slab.overdrive(shardRespChan, shards); upload != nil {
					_ = slab.launch(upload) // ignore error
				}
				resetTimeout()
			}
		}
	}()

	// collect responses
	var finished bool
	for slab.inflight() > 0 && !finished {
		var resp shardResp
		select {
		case <-mgr.stopChan:
			return nil, errors.New("upload stopped")
		case <-ctx.Done():
			return nil, ctx.Err()
		case resp = <-shardRespChan:
		}

		// receive the response
		finished = slab.receive(resp)

		// handle the response
		if resp.err == nil {
			resetTimeout()

			// signal the upload a shard was received
			select {
			case u.doneShardTrigger <- struct{}{}:
			default:
			}

			// try and trigger the next slab read
			slab.tryTriggerNextRead()
		}

		// relaunch non-overdrive uploads
		if resp.err != nil && !resp.req.overdrive {
			if err := slab.launch(resp.req); err != nil {
				break // fail the download
			}
		}
	}

	// register the amount of overdrive sectors
	span.SetAttributes(attribute.Int("overdrive", slab.overdriveCnt()))

	// track stats
	mgr.statsOverdrive.track(slab.overdrivePct())
	mgr.statsSpeed.track(float64(slab.uploadSpeed()))
	return slab.finish()
}

func (u *uploader) start(hp hostProvider, rl revisionLocker) {
outer:
	for {
		// wait for work
		select {
		case <-u.signalNewUpload:
		case <-u.stopChan:
			return
		}

		for {
			// check if we are stopped
			select {
			case <-u.stopChan:
				return
			default:
			}

			// pop the next upload
			upload := u.pop()
			if upload == nil {
				continue outer
			}

			// skip if upload is done
			if upload.done() {
				continue
			}

			// execute it
			var root types.Hash256
			start := time.Now()
			err := rl.withRevision(upload.ctx, defaultRevisionFetchTimeout, u.fcid, u.hk, u.siamuxAddr, lockingPriorityUpload, u.blockHeight(), func(rev types.FileContractRevision) error {
				var err error
				root, err = upload.execute(hp, rev)
				return err
			})

			// send the response
			if err != nil {
				upload.fail(err)
			} else {
				upload.succeed(root)
			}

			// track the error, ignore gracefully closed streams and canceled overdrives
			isErrClosedStream := errors.Is(err, mux.ErrClosedStream)
			canceledOverdrive := upload.done() && upload.overdrive && err != nil
			if !canceledOverdrive && !isErrClosedStream {
				u.track(err, time.Since(start))
			}
		}
	}
}

func (u *uploader) blockHeight() uint64 {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.bh
}

func (u *uploader) estimate() float64 {
	u.mu.Lock()
	defer u.mu.Unlock()

	// fetch average speed
	bytesPerMS := int(u.statsSpeed.percentileP90())
	if bytesPerMS == 0 {
		bytesPerMS = math.MaxInt64
	}

	outstanding := (len(u.queue) + 1) * rhpv2.SectorSize
	return float64(outstanding / bytesPerMS)
}

func (u *uploader) schedule(upload *shardUpload) {
	// decorate req
	span := trace.SpanFromContext(upload.ctx)
	span.SetAttributes(attribute.Stringer("hk", u.hk))
	span.AddEvent("enqueued")
	upload.fcid = u.fcid
	upload.hk = u.hk
	upload.siamuxAddr = u.siamuxAddr

	u.mu.Lock()
	defer u.mu.Unlock()

	// enqueue the job
	u.queue = append(u.queue, upload)

	// signal there's work
	select {
	case u.signalNewUpload <- struct{}{}:
	default:
	}
}

func (u *uploader) healthy() bool {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.consecutiveFailures == 0
}

func (u *uploader) track(err error, d time.Duration) {
	u.mu.Lock()
	defer u.mu.Unlock()
	if err != nil {
		u.consecutiveFailures++
		u.statsSpeed.track(1)
	} else {
		u.consecutiveFailures = 0
		u.statsSpeed.track(float64(rhpv2.SectorSize / d.Milliseconds()))
	}
}

func (u *uploader) updateBlockHeight(bh uint64) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.bh = bh
}

func (u *uploader) pop() *shardUpload {
	u.mu.Lock()
	defer u.mu.Unlock()

	if len(u.queue) > 0 {
		j := u.queue[0]
		u.queue[0] = nil
		u.queue = u.queue[1:]
		return j
	}
	return nil
}

func (upload *shardUpload) execute(hp hostProvider, rev types.FileContractRevision) (types.Hash256, error) {
	// fetch span from context
	span := trace.SpanFromContext(upload.ctx)
	span.AddEvent("execute")

	// create a host
	h, err := hp.newHostV3(upload.ctx, upload.fcid, upload.hk, upload.siamuxAddr)
	if err != nil {
		return types.Hash256{}, err
	}

	// upload the sector
	start := time.Now()
	root, err := h.UploadSector(upload.ctx, upload.sector, rev)
	if err != nil {
		return types.Hash256{}, err
	}

	// update span
	elapsed := time.Since(start)
	span.SetAttributes(attribute.Int64("duration", elapsed.Milliseconds()))
	span.RecordError(err)
	span.End()
	return root, nil
}

func (upload *shardUpload) succeed(root types.Hash256) {
	select {
	case <-upload.ctx.Done():
	case upload.responseChan <- shardResp{
		req:  upload,
		root: root,
	}:
	}
}

func (upload *shardUpload) fail(err error) {
	select {
	case <-upload.ctx.Done():
	case upload.responseChan <- shardResp{
		req: upload,
		err: err,
	}:
	}
}

func (upload *shardUpload) done() bool {
	select {
	case <-upload.ctx.Done():
		return true
	default:
		return false
	}
}

func (s *slabUpload) uploadSpeed() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	totalShards := len(s.sectors)
	completedShards := totalShards - len(s.remaining)
	bytes := completedShards * rhpv2.SectorSize
	ms := time.Since(s.started).Milliseconds()
	return int64(bytes) / ms
}

func (s *slabUpload) finish() ([]object.Sector, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	remaining := len(s.remaining)
	if remaining > 0 {
		return nil, fmt.Errorf("failed to upload slab: remaining=%d, inflight=%d, launched=%d uploaders=%d errors=%w", remaining, s.numInflight, s.numLaunched, s.mgr.numUploaders(), s.errs)
	}
	return s.sectors, nil
}

func (s *slabUpload) inflight() uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.numInflight
}

func (s *slabUpload) launch(req *shardUpload) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	// enqueue the job
	err := s.mgr.enqueue(req)
	if err != nil {
		span := trace.SpanFromContext(req.ctx)
		span.RecordError(err)
		span.End()
		return err
	}

	// update the state
	s.numInflight++
	s.numLaunched++
	if req.overdrive {
		s.lastOverdrive = time.Now()
		s.overdriving[req.sectorIndex]++
	}

	return nil
}

func (s *slabUpload) overdrive(responseChan chan shardResp, shards [][]byte) *shardUpload {
	s.mu.Lock()
	defer s.mu.Unlock()

	// overdrive is not kicking in yet
	if uint64(len(s.remaining)) >= s.mgr.maxOverdrive {
		return nil
	}

	// overdrive is not due yet
	if time.Since(s.lastOverdrive) < s.mgr.overdriveTimeout {
		return nil
	}

	// overdrive is maxed out
	if s.numInflight-uint64(len(s.remaining)) >= s.mgr.maxOverdrive {
		return nil
	}

	// overdrive the remaining sector with the least number of overdrives
	lowestSI := -1
	s.overdriving[lowestSI] = math.MaxInt
	for sI := range s.remaining {
		if s.overdriving[sI] < s.overdriving[lowestSI] {
			lowestSI = sI
		}
	}
	if lowestSI == -1 {
		return nil
	}

	return &shardUpload{
		upload: s.upload,
		sID:    s.sID,
		ctx:    s.remaining[lowestSI].ctx,

		overdrive:    true,
		responseChan: responseChan,

		sectorIndex: lowestSI,
		sector:      (*[rhpv2.SectorSize]byte)(shards[lowestSI]),
	}
}

func (s *slabUpload) overdriveCnt() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return int(s.numLaunched) - len(s.sectors)
}

func (s *slabUpload) overdrivePct() float64 {
	s.mu.Lock()
	defer s.mu.Unlock()

	numOverdrive := int(s.numLaunched) - len(s.sectors)
	if numOverdrive <= 0 {
		return 0
	}

	return float64(numOverdrive) / float64(len(s.sectors))
}

func (s *slabUpload) receive(resp shardResp) (finished bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	// failed reqs can't complete the upload
	s.numInflight--
	if resp.err != nil {
		s.errs = append(s.errs, &HostError{resp.req.hk, resp.err})
		return false
	}

	// redundant sectors can't complete the upload
	if s.sectors[resp.req.sectorIndex].Root != (types.Hash256{}) {
		return false
	}

	// store the sector and call cancel on the sector ctx
	s.sectors[resp.req.sectorIndex] = object.Sector{
		Host: resp.req.hk,
		Root: resp.root,
	}
	s.remaining[resp.req.sectorIndex].cancel()

	// count the sector as complete and check if we're done
	delete(s.remaining, resp.req.sectorIndex)
	return len(s.remaining) == 0
}

func (s *slabUpload) tryTriggerNextRead() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.nextReadTriggered && len(s.remaining) <= int(s.mgr.maxOverdrive) {
		select {
		case s.upload.nextReadTrigger <- struct{}{}:
			s.nextReadTriggered = true
		default:
		}
	}
}

func (a *dataPoints) percentileP90() float64 {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.p90
}

func (a *dataPoints) recompute() float64 {
	a.mu.Lock()
	defer a.mu.Unlock()

	p90, err := a.Percentile(90)
	if err != nil {
		p90 = 0
	}

	a.p90 = p90
	return p90
}

func (a *dataPoints) track(p float64) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.Float64Data[a.cnt%len(a.Float64Data)] = p
	a.cnt++
}

func (uID uploadID) String() string {
	return fmt.Sprintf("%x", uID[:])
}

func (sID slabID) String() string {
	return fmt.Sprintf("%x", sID[:])
}
