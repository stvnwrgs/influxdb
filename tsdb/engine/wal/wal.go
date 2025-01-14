/*
Package WAL implements a write ahead log optimized for write throughput
that can be put in front of the database index.

The WAL is broken into different partitions. The default number of
partitions is 5. Each partition consists of a number of segment files.
By default these files will get up to 2MB in size before a new segment
file is opened. The files are numbered and start at 1. The number
indicates the order in which the files should be read on startup to
ensure data is recovered in the same order it was written.

Partitions are flushed and compacted individually. One of the goals with
having multiple partitions was to be able to flush only a portion of the
WAL at a time.

The WAL does not flush everything in a partition when it comes time. It will
only flush series that are over a given threshold (32kb by default). The rest
will be written into a new segment file so they can be flushed later. This
is like a compaction in an LSM Tree.
*/
package wal

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"io"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/golang/snappy"
	"github.com/influxdb/influxdb/tsdb"
)

const (
	// DefaultSegmentSize of 2MB is the size at which segment files will be rolled over
	DefaultSegmentSize = 2 * 1024 * 1024

	// PartitionCount is the number of partitions in the WAL
	PartitionCount = 5

	// FileExtension is the file extension we expect for wal segments
	FileExtension = "wal"

	// MetaFileExtension is the file extension for the log files of new fields and measurements that get created
	MetaFileExtension = "meta"

	// CompactionExtension is the file extension we expect for compaction files
	CompactionExtension = "CPT"

	// MetaFlushInterval is the period after which any compressed meta data in the .meta file will get
	// flushed to the index
	MetaFlushInterval = 10 * time.Minute

	// defaultFlushCheckInterval is how often flushes are triggered automatically by the flush criteria
	defaultFlushCheckInterval = time.Second
)

// flushType indiciates why a flush and compaction are being run so the partition can
// do the appropriate type of compaction
type flushType int

const (
	// noFlush indicates that no flush or compaction are necesssary at this time
	noFlush flushType = iota
	// memoryFlush indicates that we should look for the series using the most
	// memory to flush out and compact all others
	memoryFlush
	// idleFlush indicates that we should flush all series in the parition,
	// delete all segment files and hold off on opening a new one
	idleFlush
	// thresholdFlush indicates that we should flush all series over the ReadySize
	// and compact all other series
	thresholdFlush
	// deleteFlush indicates that we're flushing because series need to be removed from the WAL
	deleteFlush
)

var (
	// ErrCompactionRunning to return if we attempt to run a compaction on a partition that is currently running one
	ErrCompactionRunning = errors.New("compaction running")

	// ErrMemoryCompactionDone gets returned if we called to flushAndCompact to free up memory
	// but a compaction has already been done to do so
	ErrMemoryCompactionDone = errors.New("compaction already run to free up memory")

	// CompactSequence is the byte sequence within a segment file that has been compacted
	// that indicates the start of a compaction marker
	CompactSequence = []byte{0xFF, 0xFF}
)

type Log struct {
	path string

	flush              chan int    // signals a background flush on the given partition
	flushCheckTimer    *time.Timer // check this often to see if a background flush should happen
	flushCheckInterval time.Duration

	// These coordinate closing and waiting for running goroutines.
	wg      sync.WaitGroup
	closing chan struct{}

	// LogOutput is the writer used by the logger.
	LogOutput io.Writer
	logger    *log.Logger

	mu         sync.RWMutex
	partitions map[uint8]*Partition

	// metaFile is the file that compressed metadata like series and fields are written to
	metaFile *os.File

	// FlushColdInterval is the period of time after which a partition will do a
	// full flush and compaction if it has been cold for writes.
	FlushColdInterval time.Duration

	// SegmentSize is the file size at which a segment file will be rotated in a partition.
	SegmentSize int64

	// MaxSeriesSize controls when a partition should get flushed to index and compacted
	// if any series in the partition has exceeded this size threshold
	MaxSeriesSize int

	// ReadySeriesSize is the minimum size a series of points must get to before getting flushed.
	ReadySeriesSize int

	// CompactionThreshold controls when a parition will be flushed. Once this
	// percentage of series in a partition are ready, a flush and compaction will be triggered.
	CompactionThreshold float64

	// PartitionSizeThreshold specifies when a partition should be forced to be flushed.
	PartitionSizeThreshold uint64

	// partitionCount is the number of separate partitions to create for the WAL.
	// Compactions happen per partition. So this number will affect what percentage
	// of the WAL gets compacted at a time. For instance, a setting of 10 means
	// we generally will be compacting about 10% of the WAL at a time.
	partitionCount uint64

	// Index is the database that series data gets flushed to once it gets compacted
	// out of the WAL.
	Index IndexWriter

	// EnableLogging specifies if detailed logs should be output
	EnableLogging bool
}

// IndexWriter is an interface for the indexed database the WAL flushes data to
type IndexWriter interface {
	// time ascending points where each byte array is:
	//   int64 time
	//   data
	WriteIndex(pointsByKey map[string][][]byte, measurementFieldsToSave map[string]*tsdb.MeasurementFields, seriesToCreate []*tsdb.SeriesCreate) error
}

func NewLog(path string) *Log {
	return &Log{
		path:  path,
		flush: make(chan int, 1),

		// these options should be overriden by any options in the config
		LogOutput:              os.Stderr,
		FlushColdInterval:      tsdb.DefaultFlushColdInterval,
		SegmentSize:            DefaultSegmentSize,
		MaxSeriesSize:          tsdb.DefaultMaxSeriesSize,
		CompactionThreshold:    tsdb.DefaultCompactionThreshold,
		PartitionSizeThreshold: tsdb.DefaultPartitionSizeThreshold,
		ReadySeriesSize:        tsdb.DefaultReadySeriesSize,
		partitionCount:         PartitionCount,
		flushCheckInterval:     defaultFlushCheckInterval,
	}
}

// Open opens and initializes the Log. Will recover from previous unclosed shutdowns
func (l *Log) Open() error {
	if err := os.MkdirAll(l.path, 0777); err != nil {
		return err
	}

	// open the metafile for writing
	if err := l.nextMetaFile(); err != nil {
		return err
	}

	// open the partitions
	l.partitions = make(map[uint8]*Partition)
	for i := uint64(1); i <= l.partitionCount; i++ {
		p, err := NewPartition(uint8(i), l.path, l.SegmentSize, l.PartitionSizeThreshold, l.ReadySeriesSize, l.FlushColdInterval, l.Index)
		if err != nil {
			return err
		}
		p.log = l
		l.partitions[uint8(i)] = p
	}
	if err := l.openPartitionFiles(); err != nil {
		return err
	}

	l.logger = log.New(l.LogOutput, "[wal] ", log.LstdFlags)

	l.flushCheckTimer = time.NewTimer(l.flushCheckInterval)

	// Start background goroutines.
	l.wg.Add(1)
	l.closing = make(chan struct{})
	go l.autoflusher(l.closing)

	return nil
}

// Cursor will return a cursor object to Seek and iterate with Next for the WAL cache for the given
func (l *Log) Cursor(key string) tsdb.Cursor {
	l.mu.RLock()
	defer l.mu.RUnlock()

	return l.partition([]byte(key)).cursor(key)
}

func (l *Log) WritePoints(points []tsdb.Point, fields map[string]*tsdb.MeasurementFields, series []*tsdb.SeriesCreate) error {
	partitionsToWrite := l.pointsToPartitions(points)

	if err := l.writeSeriesAndFields(fields, series); err != nil {
		l.logger.Println("error writing series and fields: ", err.Error())
		return err
	}

	// get it to disk
	l.mu.RLock()
	defer l.mu.RUnlock()

	for p, points := range partitionsToWrite {
		if err := p.Write(points); err != nil {
			return err
		}
	}

	return nil
}

// Flush will force a flush on all paritions
func (l *Log) Flush() error {
	l.mu.RLock()
	defer l.mu.RUnlock()

	for _, p := range l.partitions {
		if err := p.flushAndCompact(idleFlush); err != nil {
			return err
		}
	}

	return nil
}

// LoadMetadatIndex loads the new series and fields files into memory and flushes them to the BoltDB index. This function
// should be called before making a call to Open()
func (l *Log) LoadMetadataIndex(index *tsdb.DatabaseIndex, measurementFields map[string]*tsdb.MeasurementFields) error {
	metaFiles, err := l.metadataFiles()
	if err != nil {
		return err
	}

	measurementFieldsToSave := make(map[string]*tsdb.MeasurementFields)
	seriesToCreate := make([]*tsdb.SeriesCreate, 0)

	// read all the metafiles off disk
	for _, fn := range metaFiles {
		a, err := l.readMetadataFile(fn)
		if err != nil {
			return err
		}

		// loop through the seriesAndFields and add them to the index and the collection to be written to the index
		for _, sf := range a {
			for k, mf := range sf.Fields {
				measurementFieldsToSave[k] = mf

				m := index.CreateMeasurementIndexIfNotExists(string(k))
				for name, _ := range mf.Fields {
					m.SetFieldName(name)
				}
				mf.Codec = tsdb.NewFieldCodec(mf.Fields)
				measurementFields[m.Name] = mf
			}

			for _, sc := range sf.Series {
				seriesToCreate = append(seriesToCreate, sc)

				sc.Series.InitializeShards()
				index.CreateSeriesIndexIfNotExists(tsdb.MeasurementFromSeriesKey(string(sc.Series.Key)), sc.Series)
			}
		}
	}

	if err := l.Index.WriteIndex(nil, measurementFieldsToSave, seriesToCreate); err != nil {
		return err
	}

	// now remove all the old metafiles
	for _, fn := range metaFiles {
		if err := os.Remove(fn); err != nil {
			return err
		}
	}

	return nil
}

// DeleteSeries will flush the metadata that is in the WAL to the index and remove
// all series specified from the cache and the segment files in each partition. This
// will block all writes while a compaction is done against all partitions. This function
// is meant to be called by bz1 BEFORE it updates its own index, since the metadata
// is flushed here first.
func (l *Log) DeleteSeries(keys []string) error {
	// we want to stop any writes from happening to ensure the data gets cleared
	l.mu.Lock()
	defer l.mu.Unlock()

	if err := l.flushMetadata(); err != nil {
		return err
	}

	for _, p := range l.partitions {
		p.deleteSeries(keys)
	}

	return nil
}

// readMetadataFile will read the entire contents of the meta file and return a slice of the
// seriesAndFields objects that were written in. It ignores file errors since those can't be
// recovered.
func (l *Log) readMetadataFile(fileName string) ([]*seriesAndFields, error) {
	f, err := os.OpenFile(fileName, os.O_RDWR, 0666)
	if err != nil {
		return nil, err
	}

	a := make([]*seriesAndFields, 0)

	length := make([]byte, 8)
	for {
		// get the length of the compressed seriesAndFields blob
		_, err := f.Read(length)
		if err == io.EOF {
			break
		} else if err != nil {
			f.Close()
			return nil, err
		}

		dataLength := btou64(length)
		if dataLength == 0 {
			break
		}

		// read in the compressed block and decod it
		b := make([]byte, dataLength)

		_, err = f.Read(b)
		if err == io.EOF {
			break
		} else if err != nil {
			// print the error and move on since we can't recover the file
			l.logger.Println("error reading lenght of metadata: ", err.Error())
			break
		}

		buf, err := snappy.Decode(nil, b)
		if err != nil {
			// print the error and move on since we can't recover the file
			l.logger.Println("error reading compressed metadata info: ", err.Error())
			break
		}

		sf := &seriesAndFields{}
		if err := json.Unmarshal(buf, sf); err != nil {
			// print the error and move on since we can't recover the file
			l.logger.Println("error unmarshaling json for new series and fields: ", err.Error())
			break
		}

		a = append(a, sf)
	}

	if err := f.Close(); err != nil {
		return nil, err
	}

	return a, nil
}

// writeSeriesAndFields will write the compressed fields and series to the meta file. This file persists the data
// in case the server gets shutdown before the WAL has a chance to flush everything to the cache. By default this
// file is flushed on start when bz1 calls LoadMetaDataIndex
func (l *Log) writeSeriesAndFields(fields map[string]*tsdb.MeasurementFields, series []*tsdb.SeriesCreate) error {
	if len(fields) == 0 && len(series) == 0 {
		return nil
	}

	sf := &seriesAndFields{Fields: fields, Series: series}
	b, err := json.Marshal(sf)
	if err != nil {
		return err
	}
	cb := snappy.Encode(nil, b)

	l.mu.Lock()
	defer l.mu.Unlock()

	if _, err := l.metaFile.Write(u64tob(uint64(len(cb)))); err != nil {
		return err
	}

	if _, err := l.metaFile.Write(cb); err != nil {
		return err
	}

	return l.metaFile.Sync()
}

// nextMetaFile will close the current file if there is one open and open a new file to log
// metadata updates to. This function assumes that you've locked l.mu elsewhere.
func (l *Log) nextMetaFile() error {
	if l.metaFile != nil {
		if err := l.metaFile.Close(); err != nil {
			return err
		}
	}

	metaFiles, err := l.metadataFiles()
	if err != nil {
		return err
	}

	id := 0
	if len(metaFiles) > 0 {
		num := strings.Split(filepath.Base(metaFiles[len(metaFiles)-1]), ".")[0]
		n, err := strconv.ParseInt(num, 10, 32)

		if err != nil {
			return err
		}

		id = int(n)
	}

	nextFileName := filepath.Join(l.path, fmt.Sprintf("%06d.%s", id, MetaFileExtension))
	l.metaFile, err = os.OpenFile(nextFileName, os.O_CREATE|os.O_RDWR, 0666)
	if err != nil {
		return err
	}

	return nil
}

// metadataFiles returns the files in the WAL directory with the MetaFileExtension
func (l *Log) metadataFiles() ([]string, error) {
	path := filepath.Join(l.path, fmt.Sprintf("*.%s", MetaFileExtension))

	a, err := filepath.Glob(path)
	if err != nil {
		return nil, err
	}

	sort.Strings(a)

	return a, nil
}

// pointsToPartitions returns a map that organizes the points into the partitions they should be mapped to
func (l *Log) pointsToPartitions(points []tsdb.Point) map[*Partition][]tsdb.Point {
	m := make(map[*Partition][]tsdb.Point)
	for _, p := range points {
		pp := l.partition(p.Key())
		m[pp] = append(m[pp], p)
	}
	return m
}

// openPartitionFiles will open all partitions and read their segment files
func (l *Log) openPartitionFiles() error {
	results := make(chan error, len(l.partitions))
	for _, p := range l.partitions {

		go func(p *Partition) {
			// Recover from a partial compaction.
			if err := p.recoverCompactionFile(); err != nil {
				results <- fmt.Errorf("recover compaction files: %s", err)
				return
			}

			fileNames, err := p.segmentFileNames()
			if err != nil {
				results <- err
				return
			}
			for _, n := range fileNames {
				entries, err := p.readFile(n)
				if err != nil {
					results <- err
					return
				}
				for _, e := range entries {
					p.addToCache(e.key, e.data, e.timestamp)
				}
			}
			results <- nil
		}(p)
	}

	for i := 0; i < len(l.partitions); i++ {
		err := <-results
		if err != nil {
			return err
		}
	}

	return nil
}

// Close will finish any flush that is currently in process and close file handles
func (l *Log) Close() error {
	// stop the autoflushing process so it doesn't try to kick another one off
	if l.closing != nil {
		close(l.closing)
		l.closing = nil
	}

	l.wg.Wait()

	l.mu.Lock()
	defer l.mu.Unlock()

	// clear the cache
	l.partitions = nil

	return l.close()
}

// close all the open Log partitions and file handles
func (l *Log) close() error {
	for _, p := range l.partitions {
		if err := p.Close(); err != nil {
			return err
		}
	}

	if err := l.metaFile.Close(); err != nil {
		return err
	}

	l.metaFile = nil
	return nil
}

// triggerAutoFlush will flush and compact any partitions that have hit the thresholds for compaction
func (l *Log) triggerAutoFlush() {
	l.mu.RLock()
	defer l.mu.RUnlock()
	for _, p := range l.partitions {
		if f := p.shouldFlush(l.MaxSeriesSize, l.CompactionThreshold); f != noFlush {
			if err := p.flushAndCompact(f); err != nil {
				l.logger.Printf("error flushing partition %d: %s\n", p.id, err)
			}
		}
	}
}

// autoflusher waits for notification of a flush and kicks it off in the background.
// This method runs in a separate goroutine.
func (l *Log) autoflusher(closing chan struct{}) {
	defer l.wg.Done()

	metaFlushTicker := time.NewTicker(MetaFlushInterval)

	for {
		// Wait for close or flush signal.
		select {
		case <-closing:
			metaFlushTicker.Stop()
			return
		case <-l.flushCheckTimer.C:
			l.triggerAutoFlush()
			l.flushCheckTimer.Reset(l.flushCheckInterval)
		case <-l.flush:
			if err := l.Flush(); err != nil {
				l.logger.Printf("flush error: %s", err)
			}
		case <-metaFlushTicker.C:
			if err := l.flushMetadata(); err != nil {
				l.logger.Printf("metadata flush error: %s", err.Error())
			}
		}
	}
}

// flushMetadata will write start a new metafile for writes to go through and then flush all
// metadata from previous files to the index. After a sucessful write, the metadata files
// will be removed. While the flush to index is happening we aren't blocked for new metadata writes.
func (l *Log) flushMetadata() error {
	files, err := l.metadataFiles()
	if err != nil {
		return err
	}

	if err := l.nextMetaFile(); err != nil {
		return err
	}

	measurements := make(map[string]*tsdb.MeasurementFields)
	series := make([]*tsdb.SeriesCreate, 0)

	// read all the measurement fields and series from the metafiles
	for _, fn := range files {
		a, err := l.readMetadataFile(fn)
		if err != nil {
			return err
		}

		for _, sf := range a {
			for k, mf := range sf.Fields {
				measurements[k] = mf
			}

			series = append(series, sf.Series...)
		}
	}

	// write them to the index
	if err := l.Index.WriteIndex(nil, measurements, series); err != nil {
		return err
	}

	// remove the old files now that we've persisted them elsewhere
	for _, fn := range files {
		if err := os.Remove(fn); err != nil {
			return err
		}
	}

	return nil
}

// walPartition returns the partition number that key belongs to.
func (l *Log) partition(key []byte) *Partition {
	h := fnv.New64a()
	h.Write(key)
	id := uint8(h.Sum64()%l.partitionCount + 1)
	p := l.partitions[id]
	if p == nil {
		p, err := NewPartition(id, l.path, l.SegmentSize, l.PartitionSizeThreshold, l.ReadySeriesSize, l.FlushColdInterval, l.Index)
		if err != nil {
			panic(err)
		}
		p.log = l
		l.partitions[id] = p
	}
	return p
}

// Partition is a set of files for a partition of the WAL. We use multiple partitions so when compactions occur
// only a portion of the WAL must be flushed and compacted
type Partition struct {
	id                 uint8
	path               string
	mu                 sync.Mutex
	currentSegmentFile *os.File
	currentSegmentSize int64
	currentSegmentID   uint32
	lastFileID         uint32
	maxSegmentSize     int64
	cache              map[string]*cacheEntry

	index           IndexWriter
	readySeriesSize int

	// memorySize is the rough size in memory of all the cached series data
	memorySize uint64

	// sizeThreshold is the memory size after which writes start getting throttled
	sizeThreshold uint64

	// backoffCount is the number of times write has been called while memory is
	// over the threshold. It's used to gradually increase write times to put
	// backpressure on clients.
	backoffCount int

	// flushCache is a temporary placeholder to keep data while its being flushed
	// and compacted. It's for cursors to combine the cache and this if a flush is occuring
	flushCache        map[string][][]byte
	compactionRunning bool

	// flushColdInterval and lastWriteTime are used to determin if a partition should
	// be flushed because it has been idle for writes.
	flushColdInterval time.Duration
	lastWriteTime     time.Time

	log *Log

	// Used for mocking OS calls
	os struct {
		OpenCompactionFile func(name string, flag int, perm os.FileMode) (file *os.File, err error)
		OpenSegmentFile    func(name string, flag int, perm os.FileMode) (file *os.File, err error)
		Rename             func(oldpath, newpath string) error
	}
}

func NewPartition(id uint8, path string, segmentSize int64, sizeThreshold uint64, readySeriesSize int, flushColdInterval time.Duration, index IndexWriter) (*Partition, error) {
	p := &Partition{
		id:                id,
		path:              path,
		maxSegmentSize:    segmentSize,
		sizeThreshold:     sizeThreshold,
		lastWriteTime:     time.Now(),
		cache:             make(map[string]*cacheEntry),
		readySeriesSize:   readySeriesSize,
		index:             index,
		flushColdInterval: flushColdInterval,
	}

	p.os.OpenCompactionFile = os.OpenFile
	p.os.OpenSegmentFile = os.OpenFile
	p.os.Rename = os.Rename

	return p, nil
}

// Close resets the caches and closes the currently open segment file
func (p *Partition) Close() error {
	p.mu.Lock()
	defer p.mu.Unlock()

	p.cache = nil
	if err := p.currentSegmentFile.Close(); err != nil {
		return err
	}

	return nil
}

// Write will write a compressed block of the points to the current segment file. If the segment
// file is larger than the max size, it will roll over to a new file before performing the write.
// This method will also add the points to the in memory cache
func (p *Partition) Write(points []tsdb.Point) error {
	block := make([]byte, 0)
	for _, pp := range points {
		block = append(block, marshalWALEntry(pp.Key(), pp.UnixNano(), pp.Data())...)
	}
	b := snappy.Encode(nil, block)

	if backoff, ok := func() (time.Duration, bool) {
		p.mu.Lock()
		defer p.mu.Unlock()
		// pause writes for a bit if we've hit the size threshold
		if p.memorySize > p.sizeThreshold {
			p.backoffCount += 1
			return time.Millisecond * 20, true
		}

		return 0, false
	}(); ok {
		go p.flushAndCompact(memoryFlush)
		time.Sleep(backoff)
	}
	p.mu.Lock()
	defer p.mu.Unlock()

	// rotate to a new file if we've gone over our limit
	if p.currentSegmentFile == nil || p.currentSegmentSize > p.maxSegmentSize {
		err := p.newSegmentFile()
		if err != nil {
			return err
		}
	}

	if n, err := p.currentSegmentFile.Write(u64tob(uint64(len(b)))); err != nil {
		return err
	} else if n != 8 {
		return fmt.Errorf("expected to write %d bytes but wrote %d", 8, n)
	}

	if n, err := p.currentSegmentFile.Write(b); err != nil {
		return err
	} else if n != len(b) {
		return fmt.Errorf("expected to write %d bytes but wrote %d", len(b), n)
	}

	if err := p.currentSegmentFile.Sync(); err != nil {
		return err
	}

	p.currentSegmentSize += int64(8 + len(b))
	p.lastWriteTime = time.Now()

	for _, pp := range points {
		p.addToCache(pp.Key(), pp.Data(), pp.UnixNano())
	}
	return nil
}

// newSegmentFile will close the current segment file and open a new one, updating bookkeeping info on the partition
func (p *Partition) newSegmentFile() error {
	p.currentSegmentID += 1
	if p.currentSegmentFile != nil {
		if err := p.currentSegmentFile.Close(); err != nil {
			return err
		}
	}

	fileName := p.fileNameForSegment(p.currentSegmentID)
	ff, err := os.OpenFile(fileName, os.O_CREATE|os.O_RDWR, 0666)
	if err != nil {
		return err
	}
	p.currentSegmentSize = 0
	p.currentSegmentFile = ff

	return nil
}

// fileNameForSegment will return the full path and filename for a given segment ID
func (p *Partition) fileNameForSegment(id uint32) string {
	return filepath.Join(p.path, fmt.Sprintf("%02d.%06d.%s", p.id, id, FileExtension))
}

// compactionFileName is the name of the temporary file used for compaction
func (p *Partition) compactionFileName() string {
	return filepath.Join(p.path, fmt.Sprintf("%02d.%06d.%s", p.id, 1, CompactionExtension))
}

// fileIDFromName will return the segment ID from the file name
func (p *Partition) fileIDFromName(name string) (uint32, error) {
	parts := strings.Split(filepath.Base(name), ".")
	if len(parts) != 3 {
		return 0, fmt.Errorf("file name doesn't follow wal format: %s", name)
	}
	id, err := strconv.ParseUint(parts[1], 10, 32)
	if err != nil {
		return 0, err
	}
	return uint32(id), nil
}

// shouldFlush returns a flushType that indicates if a partition should be flushed and why. The criteria are:
// maxSeriesSize - flush if any series in the partition has exceeded this size threshold
// readySeriesSize - a series is ready to flush once it has this much data in it
// compactionThreshold - a partition is ready to flush if this percentage of series has hit the readySeriesSize or greater
func (p *Partition) shouldFlush(maxSeriesSize int, compactionThreshold float64) flushType {
	p.mu.Lock()
	defer p.mu.Unlock()

	if len(p.cache) == 0 {
		return noFlush
	}

	if p.memorySize > p.sizeThreshold {
		return memoryFlush
	}

	if time.Since(p.lastWriteTime) > p.flushColdInterval {
		return idleFlush
	}

	countReady := 0
	for _, c := range p.cache {
		// if we have a series with the max possible size, shortcut out because we need to flush
		if c.size > maxSeriesSize {
			return thresholdFlush
		} else if c.size > p.readySeriesSize {
			countReady += 1
		}
	}

	if float64(countReady)/float64(len(p.cache)) > compactionThreshold {
		return thresholdFlush
	}

	return noFlush
}

// prepareSeriesToFlush will empty the cache of series that are ready based on their size
// and return information for the compaction process to use.
func (p *Partition) prepareSeriesToFlush(readySeriesSize int, flush flushType) (*compactionInfo, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	// if there is either a compaction running or one just ran and relieved
	// memory pressure, just return from here
	if p.compactionRunning {
		return nil, ErrCompactionRunning
	} else if flush == memoryFlush && p.memorySize < p.sizeThreshold {
		return nil, ErrMemoryCompactionDone
	}
	p.compactionRunning = true

	// we've been ordered to flush and compact. iterate until we have at least
	// some series to flush by cutting the ready size in half each iteration
	// if we didn't come up with any
	var seriesToFlush map[string][][]byte
	var size int

	// if this flush is being triggered because the partition is idle, all series hit the threshold
	if flush == idleFlush {
		for _, c := range p.cache {
			size += c.size
		}
		seriesToFlush = make(map[string][][]byte)
		for k, c := range p.cache {
			seriesToFlush[k] = c.points
		}
		p.cache = make(map[string]*cacheEntry)
	} else {
		// only grab the series that hit the thresold. loop until we have series to flush
		for {
			s, n := p.seriesToFlush(readySeriesSize)
			if len(s) > 0 {
				seriesToFlush = s
				size += n
				break
			}
			// we didn't get any series to flush so cut the ready size in half
			// and see if there are series that are ready at that level
			readySeriesSize = readySeriesSize / 2
		}
	}

	c := &compactionInfo{seriesToFlush: seriesToFlush, flushSize: size}

	if flush == idleFlush {
		// don't create a new segment file because this partition is idle
		if p.currentSegmentFile != nil {
			if err := p.currentSegmentFile.Close(); err != nil {
				return nil, err
			}
		}
		p.currentSegmentFile = nil
		p.currentSegmentID += 1
		p.currentSegmentSize = 0
	} else {
		// roll over a new segment file so we can compact all the old ones
		if err := p.newSegmentFile(); err != nil {
			return nil, err
		}
	}

	p.flushCache = c.seriesToFlush
	c.compactFilesLessThan = p.currentSegmentID

	return c, nil
}

// seriesToFlush will clear the cache of series over the give threshold and return
// them in a new map along with their combined size
func (p *Partition) seriesToFlush(readySeriesSize int) (map[string][][]byte, int) {
	seriesToFlush := make(map[string][][]byte)
	size := 0
	for k, c := range p.cache {
		// if the series is over the threshold, save it in the map to flush later
		if c.size >= readySeriesSize {
			size += c.size
			seriesToFlush[k] = c.points

			// always hand the index data that is sorted
			if c.isDirtySort {
				sort.Sort(tsdb.ByteSlices(seriesToFlush[k]))
			}

			delete(p.cache, k)
		}
	}

	return seriesToFlush, size
}

// flushAndCompact will flush any series that are over their threshold and then read in all old segment files and
// write the data that was not flushed to a new file
func (p *Partition) flushAndCompact(flush flushType) error {
	c, err := p.prepareSeriesToFlush(p.readySeriesSize, flush)

	if err == ErrCompactionRunning || err == ErrMemoryCompactionDone {
		return nil
	} else if err != nil {
		return err
	} else if len(c.seriesToFlush) == 0 { // nothing to flush!
		return nil
	}

	startTime := time.Now()
	if p.log.EnableLogging {
		p.log.logger.Printf("compacting %d series from partition %d\n", len(c.seriesToFlush), p.id)
	}

	// write the data to the index first
	if err := p.index.WriteIndex(c.seriesToFlush, nil, nil); err != nil {
		// if we can't write the index, we should just bring down the server hard
		panic(fmt.Sprintf("error writing the wal to the index: %s", err.Error()))
	}

	// clear the flush cache and reset the memory thresholds
	p.mu.Lock()
	p.flushCache = nil
	p.memorySize -= uint64(c.flushSize)
	p.backoffCount = 0
	p.mu.Unlock()

	// ensure that we mark that compaction is no longer running
	defer func() {
		p.mu.Lock()
		p.compactionRunning = false
		p.mu.Unlock()
	}()

	err = p.compactFiles(c, flush)
	if p.log.EnableLogging {
		p.log.logger.Printf("compaction of partition %d took %s\n", p.id, time.Since(startTime))
	}

	return err
}

func (p *Partition) compactFiles(c *compactionInfo, flush flushType) error {
	// now compact all the old data
	fileNames, err := p.segmentFileNames()
	if err != nil {
		return err
	}

	// all compacted data from the segments will go into this file
	compactionFile, err := p.os.OpenCompactionFile(p.compactionFileName(), os.O_CREATE|os.O_RDWR, 0666)
	if err != nil {
		return err
	}

	for _, n := range fileNames {
		id, err := p.idFromFileName(n)
		if err != nil {
			return err
		}

		// only compact files that are older than the segment that became active when we started the flush
		if id >= c.compactFilesLessThan {
			break
		}

		f, err := p.os.OpenSegmentFile(n, os.O_RDONLY, 0666)
		if err != nil {
			return err
		}

		sf := newSegment(f)
		var entries []*entry
		for {
			name, a, err := sf.readCompressedBlock()
			if name != "" {
				continue // skip name blocks
			} else if err != nil {
				return err
			} else if a == nil {
				break
			}

			// only compact the entries from series that haven't been flushed
			for _, e := range a {
				if _, ok := c.seriesToFlush[string(e.key)]; !ok {
					entries = append(entries, e)
				}
			}
		}

		if err := p.writeCompactionEntry(compactionFile, f.Name(), entries); err != nil {
			return err
		}

		// now close and delete the file
		if err := f.Close(); err != nil {
			return err
		}

		if err := os.Remove(n); err != nil {
			return err
		}
	}

	// close the compaction file and rename it so that it will appear as the very first segment
	if err := compactionFile.Close(); err != nil {
		return err
	}

	// if it's an idle flush remove the compaction file
	if flush == idleFlush {
		return os.Remove(compactionFile.Name())
	}

	return p.os.Rename(compactionFile.Name(), p.fileNameForSegment(1))
}

// writeCompactionEntry will write a marker for the beginning of the file we're compacting, a compressed block
// for all entries, then a marker for the end of the file
func (p *Partition) writeCompactionEntry(f *os.File, filename string, entries []*entry) error {
	if err := p.writeCompactionFileName(f, filename); err != nil {
		return err
	}

	block := make([]byte, 0)
	for _, e := range entries {
		block = append(block, marshalWALEntry(e.key, e.timestamp, e.data)...)
	}

	b := snappy.Encode(nil, block)
	if _, err := f.Write(u64tob(uint64(len(b)))); err != nil {
		return err
	}

	if _, err := f.Write(b); err != nil {
		return err
	}

	return f.Sync()
}

// writeCompactionFileName will write a compaction log length entry and the name of the file that is compacted
func (p *Partition) writeCompactionFileName(f *os.File, filename string) error {
	length := u64tob(uint64(len([]byte(filename))))

	// the beginning of the length has two bytes to indicate that this is a compaction log entry
	length[0] = 0xFF
	length[1] = 0xFF

	if _, err := f.Write(length); err != nil {
		return err
	}

	if _, err := f.Write([]byte(filename)); err != nil {
		return err
	}

	return nil
}

// recoverCompactionFile iterates over all compaction files in a directory and
// cleans them and removes undeleted files.
func (p *Partition) recoverCompactionFile() error {
	path := p.compactionFileName()

	// Open compaction file. Ignore if it doesn't exist.
	f, err := p.os.OpenCompactionFile(path, os.O_RDWR, 0666)
	if os.IsNotExist(err) {
		return nil
	} else if err != nil {
		return err
	}
	defer f.Close()

	// Iterate through all named blocks.
	sf := newSegment(f)
	var hasData bool
	for {
		// Only read named blocks.
		name, a, err := sf.readCompressedBlock()
		if err != nil {
			return fmt.Errorf("read name block: %s", err)
		} else if name == "" && a == nil {
			break // eof
		} else if name == "" {
			continue // skip unnamed blocks
		}

		// Read data for the named block.
		if s, entries, err := sf.readCompressedBlock(); err != nil {
			return fmt.Errorf("read data block: %s", err)
		} else if s != "" {
			return fmt.Errorf("unexpected double name block")
		} else if entries == nil {
			break // eof
		}

		// If data exists then ensure the underlying segment is deleted.
		if err := os.Remove(name); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("remove segment: filename=%s, err=%s", name, err)
		}

		// Flag the compaction file as having data and it should be renamed.
		hasData = true
	}
	f.Close()

	// If the compaction file did not have at least one named block written to
	// it then it should removed. This check is performed to ensure a partial
	// compaction file does not overwrite an original segment file.
	if !hasData {
		if err := os.Remove(path); err != nil {
			return fmt.Errorf("remove compaction file: %s", err)
		}
		return nil
	}

	// Double check that we are not renaming the compaction file over an
	// existing segment file. The segment file should be removed in the
	// recovery process but this simply double checks that removal occurred.
	newpath := p.fileNameForSegment(1)
	if _, err := os.Stat(newpath); !os.IsNotExist(err) {
		return fmt.Errorf("cannot rename compaction file, segment exists: filename=%s", newpath)
	}

	// Rename compaction file to the first segment file.
	if err := p.os.Rename(path, newpath); err != nil {
		return fmt.Errorf("rename compaction file: %s", err)
	}

	return nil
}

// readFile will read a segment file and marshal its entries into the cache
func (p *Partition) readFile(path string) (entries []*entry, err error) {
	id, err := p.fileIDFromName(path)
	if err != nil {
		return nil, err
	}

	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0666)
	if err != nil {
		return nil, err
	}

	sf := newSegment(f)
	for {
		name, a, err := sf.readCompressedBlock()
		if name != "" {
			continue // skip name blocks
		} else if err != nil {
			f.Close()
			return nil, err
		} else if a == nil {
			break
		}

		entries = append(entries, a...)
	}

	// if this is the highest segment file, it'll be the one we use, otherwise close it out now that we're done reading
	if id > p.currentSegmentID {
		p.currentSegmentID = id
		p.currentSegmentFile = f
		p.currentSegmentSize = sf.size
	} else {
		if err := f.Close(); err != nil {
			return nil, err
		}
	}
	return
}

// addToCache will marshal the entry and add it to the in memory cache. It will also mark if this key will need sorting later
func (p *Partition) addToCache(key, data []byte, timestamp int64) {
	// Generate in-memory cache entry of <timestamp,data>.
	v := MarshalEntry(timestamp, data)
	p.memorySize += uint64(len(v))

	entry := p.cache[string(key)]
	if entry == nil {
		entry = &cacheEntry{
			points: [][]byte{v},
			size:   len(v),
		}
		p.cache[string(key)] = entry

		return
	}

	// Determine if we'll need to sort the values for this key later
	entry.isDirtySort = bytes.Compare(entry.points[len(entry.points)-1][0:8], v[0:8]) != -1
	entry.points = append(entry.points, v)
	entry.size += len(v)
}

// cursor will combine the in memory cache and flush cache (if a flush is currently happening) to give a single ordered cursor for the key
func (p *Partition) cursor(key string) *cursor {
	p.mu.Lock()
	defer p.mu.Unlock()

	entry := p.cache[key]
	if entry == nil {
		return &cursor{}
	}

	// if we're in the middle of a flush, combine the previous cache
	// with this one for the cursor
	if p.flushCache != nil {
		if fc, ok := p.flushCache[key]; ok {
			c := make([][]byte, len(fc), len(fc)+len(entry.points))
			copy(c, fc)
			c = append(c, entry.points...)

			return &cursor{cache: tsdb.DedupeEntries(c)}
		}
	}

	if entry.isDirtySort {
		entry.points = tsdb.DedupeEntries(entry.points)
		entry.isDirtySort = false
	}

	// build a copy so modifications to the partition don't change the result set
	a := make([][]byte, len(entry.points))
	copy(a, entry.points)
	return &cursor{cache: a}
}

// idFromFileName parses the segment file ID from its name
func (p *Partition) idFromFileName(name string) (uint32, error) {
	parts := strings.Split(filepath.Base(name), ".")
	if len(parts) != 3 {
		return 0, fmt.Errorf("file %s has wrong name format to be a segment file", name)
	}

	id, err := strconv.ParseUint(parts[1], 10, 32)

	return uint32(id), err
}

// segmentFileNames returns all the segment files names for the partition
func (p *Partition) segmentFileNames() ([]string, error) {
	path := filepath.Join(p.path, fmt.Sprintf("%02d.*.%s", p.id, FileExtension))
	return filepath.Glob(path)
}

// deleteSeries will perform a compaction on the partition, removing all data
// from any of the series passed in.
func (p *Partition) deleteSeries(keys []string) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	p.compactionRunning = true

	// remove the series from the cache and prepare the compaction info
	size := 0
	seriesToFlush := make(map[string][][]byte)
	for _, k := range keys {
		entry := p.cache[k]
		if entry != nil {
			seriesToFlush[k] = entry.points
			size += entry.size
			delete(p.cache, k)
		}
	}
	p.memorySize -= uint64(size)

	c := &compactionInfo{seriesToFlush: seriesToFlush, flushSize: size}

	// roll over a new segment file so we can compact all the old ones
	if err := p.newSegmentFile(); err != nil {
		return err
	}
	c.compactFilesLessThan = p.currentSegmentID

	return p.compactFiles(c, deleteFlush)
}

// compactionInfo is a data object with information about a compaction running
// and the series that will be flushed to the index
type compactionInfo struct {
	seriesToFlush        map[string][][]byte
	compactFilesLessThan uint32
	flushSize            int
}

// segmentFile is a struct for reading in segment files from the WAL. Used on startup only while loading
type segment struct {
	f      *os.File
	block  []byte
	length []byte
	size   int64
}

func newSegment(f *os.File) *segment {
	return &segment{
		length: make([]byte, 8),
		f:      f,
	}
}

// readCompressedBlock will read the next compressed block from the file and marshal the entries.
// if we've hit the end of the file or corruption the entry array will be nil
func (s *segment) readCompressedBlock() (name string, entries []*entry, err error) {
	blockSize := int64(0)

	n, err := s.f.Read(s.length)
	if err == io.EOF {
		return "", nil, nil
	} else if err != nil {
		return "", nil, fmt.Errorf("read length: %s", err)
	} else if n != len(s.length) {
		// seek back before this length so we can start overwriting the file from here
		log.Println("unable to read the size of a data block from file: ", s.f.Name())
		s.f.Seek(-int64(n), 1)
		return "", nil, nil
	}
	blockSize += int64(n)

	// Compacted WAL files will have a magic byte sequence that indicate the next part is a file name
	// instead of a compressed block. We can ignore these bytes and the ensuing file name to get to the next block.
	isCompactionFileNameBlock := bytes.Equal(s.length[0:2], CompactSequence)
	if isCompactionFileNameBlock {
		s.length[0], s.length[1] = 0x00, 0x00
	}

	dataLength := btou64(s.length)

	// make sure we haven't hit the end of data. trailing end of file can be zero bytes
	if dataLength == 0 {
		s.f.Seek(-int64(len(s.length)), 1)
		return "", nil, nil
	}

	if len(s.block) < int(dataLength) {
		s.block = make([]byte, dataLength)
	}

	n, err = s.f.Read(s.block[:dataLength])
	if err != nil {
		return "", nil, fmt.Errorf("read block: %s", err)
	}
	blockSize += int64(n)

	// read the compressed block and decompress it. if partial or corrupt,
	// overwrite with zeroes so we can start over on this wal file
	if n != int(dataLength) {
		log.Println("partial compressed block in file: ", s.f.Name())

		// seek back to before this block and its size so we can overwrite the corrupt data
		s.f.Seek(-int64(len(s.length)+n), 1)
		if err := s.f.Truncate(s.size); err != nil {
			return "", nil, fmt.Errorf("truncate(0): sz=%d, err=%s", s.size, err)
		}

		return "", nil, nil
	}

	// skip the rest if this is just the filename from a compaction
	if isCompactionFileNameBlock {
		return string(s.block[:dataLength]), nil, nil
	}

	// if there was an error decoding, this is a corrupt block so we zero out the rest of the file
	buf, err := snappy.Decode(nil, s.block[:dataLength])
	if err != nil {
		log.Println("corrupt compressed block in file: ", err.Error(), s.f.Name())

		// go back to the start of this block and zero out the rest of the file
		s.f.Seek(-int64(len(s.length)+n), 1)
		if err := s.f.Truncate(s.size); err != nil {
			return "", nil, fmt.Errorf("truncate(1): sz=%d, err=%s", s.size, err)
		}

		return "", nil, nil
	}

	// read in the individual data points from the decompressed wal block
	bytesRead := 0
	entries = make([]*entry, 0)
	for {
		if bytesRead >= len(buf) {
			break
		}
		n, key, timestamp, data := unmarshalWALEntry(buf[bytesRead:])
		bytesRead += n
		entries = append(entries, &entry{key: key, data: data, timestamp: timestamp})
	}

	s.size = blockSize

	return
}

// entry is used as a temporary object when reading data from segment files
type entry struct {
	key       []byte
	data      []byte
	timestamp int64
}

// cursor is a forward cursor for a given entry in the cache
type cursor struct {
	cache    [][]byte
	position int
}

// Seek will point the cursor to the given time (or key)
func (c *cursor) Seek(seek []byte) (key, value []byte) {
	// Seek cache index.
	c.position = sort.Search(len(c.cache), func(i int) bool {
		return bytes.Compare(c.cache[i][0:8], seek) != -1
	})

	return c.Next()
}

// Next moves the cursor to the next key/value. will return nil if at the end
func (c *cursor) Next() (key, value []byte) {
	if c.position >= len(c.cache) {
		return nil, nil
	}

	v := c.cache[c.position]
	c.position++

	return v[0:8], v[8:]

}

// seriesAndFields is a data struct to serialize new series and fields
// to get created into WAL segment files
type seriesAndFields struct {
	Fields map[string]*tsdb.MeasurementFields `json:"fields,omitempty"`
	Series []*tsdb.SeriesCreate               `json:"series,omitempty"`
}

// cacheEntry holds the cached data for a series
type cacheEntry struct {
	points      [][]byte
	isDirtySort bool
	size        int
}

// marshalWALEntry encodes point data into a single byte slice.
//
// The format of the byte slice is:
//
//     uint64 timestamp
//     uint32 key length
//     uint32 data length
//     []byte key
//     []byte data
//
func marshalWALEntry(key []byte, timestamp int64, data []byte) []byte {
	v := make([]byte, 8+4+4, 8+4+4+len(key)+len(data))
	binary.BigEndian.PutUint64(v[0:8], uint64(timestamp))
	binary.BigEndian.PutUint32(v[8:12], uint32(len(key)))
	binary.BigEndian.PutUint32(v[12:16], uint32(len(data)))

	v = append(v, key...)
	v = append(v, data...)

	return v
}

// unmarshalWALEntry decodes a WAL entry into it's separate parts.
// Returned byte slices point to the original slice.
func unmarshalWALEntry(v []byte) (bytesRead int, key []byte, timestamp int64, data []byte) {
	timestamp = int64(binary.BigEndian.Uint64(v[0:8]))
	keyLen := binary.BigEndian.Uint32(v[8:12])
	dataLen := binary.BigEndian.Uint32(v[12:16])

	key = v[16 : 16+keyLen]
	data = v[16+keyLen : 16+keyLen+dataLen]
	bytesRead = 16 + int(keyLen) + int(dataLen)
	return
}

// marshalCacheEntry encodes the timestamp and data to a single byte slice.
//
// The format of the byte slice is:
//
//     uint64 timestamp
//     []byte data
//
func MarshalEntry(timestamp int64, data []byte) []byte {
	buf := make([]byte, 8+len(data))
	binary.BigEndian.PutUint64(buf[0:8], uint64(timestamp))
	copy(buf[8:], data)
	return buf
}

// unmarshalCacheEntry returns the timestamp and data from an encoded byte slice.
func UnmarshalEntry(buf []byte) (timestamp int64, data []byte) {
	timestamp = int64(binary.BigEndian.Uint64(buf[0:8]))
	data = buf[8:]
	return
}

// u64tob converts a uint64 into an 8-byte slice.
func u64tob(v uint64) []byte {
	b := make([]byte, 8)
	binary.BigEndian.PutUint64(b, v)
	return b
}

func btou64(b []byte) uint64 {
	return binary.BigEndian.Uint64(b)
}
