package renter

import (
	"bytes"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/NebulousLabs/Sia/crypto"
	"github.com/NebulousLabs/Sia/modules"
	"github.com/NebulousLabs/Sia/types"

	"github.com/NebulousLabs/errors"
)

// downloadPieceInfo contains all the information required to download and
// recover a piece of a chunk from a host. It is a value in a map where the key
// is the file contract id.
type downloadPieceInfo struct {
	index uint64
	root  crypto.Hash
}

// unfinishedDownloadChunk contains a chunk for a download that is in progress.
//
// TODO: Explore making workersStandby a heap sorted by latency or whatever
// other metric the download is prioritizing (price, total system throughput,
// etc.)
type unfinishedDownloadChunk struct {
	// Fetch + Write instructions - read only or otherwise thread safe.
	destination downloadDestination // Where to write the recovered logical chunk.
	erasureCode modules.ErasureCoder
	masterKey   crypto.TwofishKey

	// Fetch + Write instructions - read only or otherwise thread safe.
	staticChunkIndex  uint64                                     // Required for deriving the encryption keys for each piece.
	staticChunkMap    map[types.FileContractID]downloadPieceInfo // Maps from file contract ids to the info for the piece associated with that contract
	staticChunkSize   uint64
	staticFetchLength uint64 // Length within the logical chunk to fetch.
	staticFetchOffset uint64 // Offset within the logical chunk that is being downloaded.
	staticPieceSize   uint64
	staticWriteOffset int64 // Offet within the writer to write the completed data.

	// Fetch + Write instructions - read only or otherwise thread safe.
	staticLatencyTarget uint64
	staticNeedsMemory   bool // Set to true if memory was not pre-allocated for this chunk.
	staticOverdrive     int
	staticPriority      uint64

	// Download chunk state - need mutex to access.
	failed            bool      // Indicates if the chunk has been marked as failed.
	physicalChunkData [][]byte  // Used to recover the logical data.
	pieceUsage        []bool    // Which pieces are being actively fetched.
	piecesCompleted   int       // Number of pieces that have successfully completed.
	piecesRegistered  int       // Number of pieces that workers are actively fetching.
	recoveryComplete  bool      // Whether or not the recovery has completed and the chunk memory released.
	workersRemaining  int       // Number of workers still able to fetch the chunk.
	workersStandby    []*worker // Set of workers that are able to work on this download, but are not needed unless other workers fail.

	// Memory management variables.
	memoryAllocated uint64

	// The download object, mostly to update download progress.
	download *download
	mu       sync.Mutex
}

// fail will set the chunk status to failed. The physical chunk memory will be
// wiped and any allocation will be returned to the renter. The download as a
// whole will be failed as well.
func (udc *unfinishedDownloadChunk) fail(err error) {
	udc.failed = true
	udc.recoveryComplete = true
	for i := range udc.physicalChunkData {
		udc.physicalChunkData[i] = nil
	}
	udc.download.managedFail(fmt.Errorf("chunk %v failed", udc.staticChunkIndex))
}

// threadedRecoverLogicalData will take all of the pieces that have been
// downloaded and encode them into the logical data which is then written to the
// underlying writer for the download.
func (udc *unfinishedDownloadChunk) threadedRecoverLogicalData() error {
	// Decrypt the chunk pieces.
	udc.mu.Lock()
	for i := range udc.physicalChunkData {
		// Skip empty pieces.
		if udc.physicalChunkData[i] == nil {
			continue
		}

		key := deriveKey(udc.masterKey, udc.staticChunkIndex, uint64(i))
		decryptedPiece, err := key.DecryptBytes(udc.physicalChunkData[i])
		if err != nil {
			udc.fail(err)
			udc.mu.Unlock()
			return err
		}
		udc.physicalChunkData[i] = decryptedPiece
	}

	// Recover the pieces into the logical chunk data.
	recoverWriter := new(bytes.Buffer)
	err := udc.erasureCode.Recover(udc.physicalChunkData, udc.staticChunkSize, recoverWriter)
	if err != nil {
		udc.fail(err)
		udc.mu.Unlock()
		return errors.AddContext(err, "unable to recover chunk")
	}
	// Clear out the physical chunk pieces, we do not need them anymore.
	for i := range udc.physicalChunkData {
		udc.physicalChunkData[i] = nil
	}
	udc.mu.Unlock()

	// Write the bytes to the requested output.
	start := udc.staticFetchOffset
	end := udc.staticFetchOffset+udc.staticFetchLength
	_, err = udc.destination.WriteAt(recoverWriter.Bytes()[start:end], udc.staticWriteOffset)
	if err != nil {
		udc.fail(err)
		return errors.AddContext(err, "unable to write to download destination")
	}
	recoverWriter = nil

	// Now that the download has completed and been flushed from memory, we can
	// release the memory that was used to store the data. Call 'cleanUp' to
	// trigger the memory cleanup along with some extra checks that everything
	// is consistent.
	udc.mu.Lock()
	udc.recoveryComplete = true
	udc.cleanUp()
	udc.mu.Unlock()

	// Update the download and signal completion of this chunk.
	udc.download.mu.Lock()
	defer udc.download.mu.Unlock()
	udc.download.chunksRemaining--
	atomic.AddUint64(&udc.download.atomicDataReceived, udc.staticFetchLength)
	if udc.download.chunksRemaining == 0 {
		// Download is complete, send out a notification and close the
		// destination writer.
		udc.download.endTime = time.Now()
		close(udc.download.completeChan)
		return udc.download.destination.Close()
	}
	return nil
}
