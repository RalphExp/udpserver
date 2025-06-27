// The MIT License (MIT)
//
// Copyright (c) 2015 xtaci
//
// Permission is hereby granted, free of charge, to any person obtaining a copy
// of this software and associated documentation files (the "Software"), to deal
// in the Software without restriction, including without limitation the rights
// to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
// copies of the Software, and to permit persons to whom the Software is
// furnished to do so, subject to the following conditions:
//
// The above copyright notice and this permission notice shall be included in all
// copies or substantial portions of the Software.
//
// THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
// IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
// FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
// AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
// LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
// OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE
// SOFTWARE.

// THE GENERALIZED REED-SOLOMON FEC SCHEME
//
// Encoding:
// -----------
// Message:         | M1 | M2 | M3 | M4 |
// Generate Parity: | P1 | P2 |
// Encoded Codeword:| M1 | M2 | M3 | M4 | P1 | P2 |
//
// Decoding with Erasures:
// ------------------------
// Received:        | M1 | ?? | M3 | M4 | P1 | ?? |
// Erasures:        |    | E1 |    |    |    | E2 |
// Syndromes:       S1, S2, ...
// Error Locator:   Λ(x) = ...
// Correct Erasures:Determine values for E1 (M2) and E2 (P2).
// Corrected:       | M1 | M2 | M3 | M4 | P1 | P2 |

package fec

import (
	"encoding/binary"
	"sync/atomic"
	"time"

	"github.com/klauspost/reedsolomon"
)

const (
	FecHeaderSize      = 6
	FecHeaderSizePlus2 = FecHeaderSize + 2 // plus 2B data size
	TypeData           = 0xf1
	TypeParity         = 0xf2
	fecExpire          = 60000
	rxFECMulti         = 3 // FEC keeps rxFECMulti* (dataShard+parityShard) ordered packets in memory
)

// FecPacket is a decoded FEC packet
type FecPacket []byte

func (bts FecPacket) Seqid() uint32 { return binary.LittleEndian.Uint32(bts) }
func (bts FecPacket) Flag() uint16  { return binary.LittleEndian.Uint16(bts[4:]) }
func (bts FecPacket) Data() []byte  { return bts[FecHeaderSize:] }

// FecElement has auxcilliary time field
type FecElement struct {
	FecPacket
	ts uint32
}

// FecDecoder for decoding incoming packets
type FecDecoder struct {
	rxlimit      int // queue size limit
	dataShards   int
	parityShards int
	shardSize    int
	rx           []FecElement // ordered receive queue

	// caches
	decodeCache [][]byte
	flagCache   []bool

	// RS decoder
	codec reedsolomon.Encoder

	// auto tune fec parameter
	autoTune   autoTune
	shouldTune bool
}

func NewFECDecoder(dataShards, parityShards int) *FecDecoder {
	if dataShards <= 0 || parityShards <= 0 {
		return nil
	}

	dec := new(FecDecoder)
	dec.dataShards = dataShards
	dec.parityShards = parityShards
	dec.shardSize = dataShards + parityShards
	dec.rxlimit = rxFECMulti * dec.shardSize
	codec, err := reedsolomon.New(dataShards, parityShards)
	if err != nil {
		return nil
	}
	dec.codec = codec
	dec.decodeCache = make([][]byte, dec.shardSize)
	dec.flagCache = make([]bool, dec.shardSize)
	return dec
}

// decode a fec packet
func (dec *FecDecoder) Decode(in FecPacket) (recovered [][]byte) {
	// sample to auto FEC tuner
	if in.Flag() == TypeData {
		dec.autoTune.Sample(true, in.Seqid())
	} else {
		dec.autoTune.Sample(false, in.Seqid())
	}

	// check if FEC parameters is out of sync
	if int(in.Seqid())%dec.shardSize < dec.dataShards {
		if in.Flag() != TypeData { // expect typeData
			dec.shouldTune = true
		}
	} else {
		if in.Flag() != TypeParity {
			dec.shouldTune = true
		}
	}

	// if signal is out-of-sync, try to detect the pattern in the signal
	if dec.shouldTune {
		autoDS := dec.autoTune.FindPeriod(true)
		autoPS := dec.autoTune.FindPeriod(false)

		// edges found, we can tune parameters now
		if autoDS > 0 && autoPS > 0 && autoDS < 256 && autoPS < 256 {
			// and make sure it's different
			if autoDS != dec.dataShards || autoPS != dec.parityShards {
				dec.dataShards = autoDS
				dec.parityShards = autoPS
				dec.shardSize = autoDS + autoPS
				dec.rxlimit = rxFECMulti * dec.shardSize
				codec, err := reedsolomon.New(autoDS, autoPS)
				if err != nil {
					return nil
				}
				dec.codec = codec
				dec.decodeCache = make([][]byte, dec.shardSize)
				dec.flagCache = make([]bool, dec.shardSize)
				dec.shouldTune = false
				//log.Println("autotune to :", dec.dataShards, dec.parityShards)
			}
		}
	}

	// parameters in tuning
	if dec.shouldTune {
		return nil
	}

	// insertion
	n := len(dec.rx) - 1
	insertIdx := 0
	for i := n; i >= 0; i-- {
		if in.Seqid() == dec.rx[i].Seqid() { // de-duplicate
			return nil
		} else if _itimediff(in.Seqid(), dec.rx[i].Seqid()) > 0 { // insertion
			insertIdx = i + 1
			break
		}
	}

	// make a copy
	pkt := FecPacket(xmitBuf.Get().([]byte)[:len(in)])
	copy(pkt, in)
	elem := FecElement{pkt, currentMs()}

	// insert into ordered rx queue
	if insertIdx == n+1 {
		dec.rx = append(dec.rx, elem)
	} else {
		dec.rx = append(dec.rx, FecElement{})
		copy(dec.rx[insertIdx+1:], dec.rx[insertIdx:]) // shift right
		dec.rx[insertIdx] = elem
	}

	// shard range for current packet
	// NOTE: the shard sequence number starts at 0, so we can use mod operation
	// to find the beginning of the current shard.
	// ALWAYS ALIGNED TO 0
	shardBegin := pkt.Seqid() - pkt.Seqid()%uint32(dec.shardSize)
	shardEnd := shardBegin + uint32(dec.shardSize) - 1

	// Define max search range in ordered queue for current shard
	searchBegin := insertIdx - int(pkt.Seqid()%uint32(dec.shardSize))
	if searchBegin < 0 {
		searchBegin = 0
	}
	searchEnd := searchBegin + dec.shardSize - 1
	if searchEnd >= len(dec.rx) {
		searchEnd = len(dec.rx) - 1
	}

	// check if we have enough shards to recover, if so, we can recover the data and free the shards
	// if not, we can keep the shards in memory for future recovery.
	if searchEnd-searchBegin+1 >= dec.dataShards {
		var numshard, numDataShard, first, maxlen int

		// zero working set for decoding
		shards := dec.decodeCache
		shardsflag := dec.flagCache
		for k := range dec.decodeCache {
			shards[k] = nil
			shardsflag[k] = false
		}

		// lookup shards in range [searchBegin, searchEnd] to the working set
		for i := searchBegin; i <= searchEnd; i++ {
			seqid := dec.rx[i].Seqid()
			// the shard seqid must be in [shardBegin, shardEnd], i.e. the current FEC group
			if _itimediff(seqid, shardEnd) > 0 {
				break
			} else if _itimediff(seqid, shardBegin) >= 0 {
				shards[seqid%uint32(dec.shardSize)] = dec.rx[i].Data()
				shardsflag[seqid%uint32(dec.shardSize)] = true
				numshard++
				if dec.rx[i].Flag() == TypeData {
					numDataShard++
				}
				if numshard == 1 {
					first = i
				}
				if len(dec.rx[i].Data()) > maxlen {
					maxlen = len(dec.rx[i].Data())
				}
			}
		}

		// case 1: if there's no loss on data shards
		if numDataShard == dec.dataShards {
			dec.rx = dec.freeRange(first, numshard, dec.rx)
		} else if numshard >= dec.dataShards { // case 2: loss on data shards, but it's recoverable from parity shards
			// make the bytes length of each shard equal
			for k := range shards {
				if shards[k] != nil {
					dlen := len(shards[k])
					shards[k] = shards[k][:maxlen]
					clear(shards[k][dlen:])
				} else if k < dec.dataShards {
					// prepare memory for the data recovery
					shards[k] = xmitBuf.Get().([]byte)[:0]
				}
			}

			// Reed-Solomon recovery
			if err := dec.codec.ReconstructData(shards); err == nil {
				for k := range shards[:dec.dataShards] {
					if !shardsflag[k] {
						// recovered data should be recycled
						recovered = append(recovered, shards[k])
					}
				}
			}

			// Free the shards in FIFO immediately
			dec.rx = dec.freeRange(first, numshard, dec.rx)
		}
	}

	// keep rxlimit in FIFO order
	if len(dec.rx) > dec.rxlimit {
		if dec.rx[0].Flag() == TypeData {
			// track the effectiveness of FEC
			atomic.AddUint64(&DefaultSnmp.FECShortShards, 1)
		}
		dec.rx = dec.freeRange(0, 1, dec.rx)
	}

	// FIFO timeout policy
	current := currentMs()
	numExpired := 0
	for k := range dec.rx {
		if _itimediff(current, dec.rx[k].ts) > fecExpire {
			numExpired++
			continue
		}
		break
	}
	if numExpired > 0 {
		dec.rx = dec.freeRange(0, numExpired, dec.rx)
	}
	return
}

// free a range of fecPacket
func (dec *FecDecoder) freeRange(first, n int, q []FecElement) []FecElement {
	for i := first; i < first+n; i++ { // recycle buffer
		xmitBuf.Put([]byte(q[i].FecPacket))
	}

	// if n is small, we can avoid the copy
	if first == 0 && n < cap(q)/2 {
		return q[n:]
	}

	// on the other hand, we shift the tail
	copy(q[first:], q[first+n:])
	return q[:len(q)-n]
}

// release all segments back to xmitBuf
func (dec *FecDecoder) release() {
	if n := len(dec.rx); n > 0 {
		dec.rx = dec.freeRange(0, n, dec.rx)
	}
}

type (
	// FecEncoder for encoding outgoing packets
	FecEncoder struct {
		dataShards   int
		parityShards int
		shardSize    int
		paws         uint32 // Protect Against Wrapped Sequence numbers
		next         uint32 // next seqid

		shardCount int // count the number of datashards collected
		maxSize    int // track maximum data length in datashard

		headerOffset  int // FEC header offset
		payloadOffset int // FEC payload offset

		// caches
		shardCache     [][]byte
		encodeCache    [][]byte
		tsLatestPacket int64

		// RS encoder
		codec reedsolomon.Encoder
	}
)

func NewFECEncoder(dataShards, parityShards, offset int) *FecEncoder {
	if dataShards <= 0 || parityShards <= 0 {
		return nil
	}
	enc := new(FecEncoder)
	enc.dataShards = dataShards
	enc.parityShards = parityShards
	enc.shardSize = dataShards + parityShards
	enc.paws = 0xffffffff / uint32(enc.shardSize) * uint32(enc.shardSize)
	enc.headerOffset = offset
	enc.payloadOffset = enc.headerOffset + FecHeaderSize

	codec, err := reedsolomon.New(dataShards, parityShards)
	if err != nil {
		return nil
	}
	enc.codec = codec

	// caches
	enc.encodeCache = make([][]byte, enc.shardSize)
	enc.shardCache = make([][]byte, enc.shardSize)
	for k := range enc.shardCache {
		enc.shardCache[k] = make([]byte, mtuLimit)
	}
	return enc
}

// encodes the packet, outputs parity shards if we have collected quorum datashards
// notice: the contents of 'ps' will be re-written in successive calling
func (enc *FecEncoder) Encode(b []byte, rto uint32) (ps [][]byte) {
	// The header format:
	// | FEC SEQID(4B) | FEC TYPE(2B) | SIZE (2B) | PAYLOAD(SIZE-2) |
	// |<-headerOffset                |<-payloadOffset
	enc.markData(b[enc.headerOffset:])
	binary.LittleEndian.PutUint16(b[enc.payloadOffset:], uint16(len(b[enc.payloadOffset:])))

	// copy data from payloadOffset to fec shard cache
	sz := len(b)
	enc.shardCache[enc.shardCount] = enc.shardCache[enc.shardCount][:sz]
	copy(enc.shardCache[enc.shardCount][enc.payloadOffset:], b[enc.payloadOffset:])
	enc.shardCount++

	// track max datashard length
	if sz > enc.maxSize {
		enc.maxSize = sz
	}

	// Generation of Reed-Solomon Erasure Code
	now := time.Now().UnixMilli()
	if enc.shardCount == enc.dataShards {
		// generate the rs-code only if the data is continuous.
		if now-enc.tsLatestPacket < int64(rto) {
			// fill '0' into the tail of each datashard
			for i := 0; i < enc.dataShards; i++ {
				shard := enc.shardCache[i]
				slen := len(shard)
				clear(shard[slen:enc.maxSize])
			}

			// construct equal-sized slice with stripped header
			cache := enc.encodeCache
			for k := range cache {
				cache[k] = enc.shardCache[k][enc.payloadOffset:enc.maxSize]
			}

			// encoding
			if err := enc.codec.Encode(cache); err == nil {
				ps = enc.shardCache[enc.dataShards:]
				for k := range ps {
					enc.markParity(ps[k][enc.headerOffset:])
					ps[k] = ps[k][:enc.maxSize]
				}
			} else {
				// record the error, and still keep the seqid monotonic increasing
				atomic.AddUint64(&DefaultSnmp.FECErrs, 1)
				enc.next = (enc.next + uint32(enc.parityShards)) % enc.paws
			}
		} else {
			// through we do not send non-continuous parity shard, we still increase the next value
			// to keep the seqid aligned with 0 start
			enc.next = (enc.next + uint32(enc.parityShards)) % enc.paws
		}

		// counters resetting
		enc.shardCount = 0
		enc.maxSize = 0
	}

	enc.tsLatestPacket = now

	return
}

// put a stamp on the FEC packet header with seqid and type
func (enc *FecEncoder) markData(data []byte) {
	binary.LittleEndian.PutUint32(data, enc.next)
	binary.LittleEndian.PutUint16(data[4:], TypeData)
	enc.next = (enc.next + 1) % enc.paws
}

func (enc *FecEncoder) markParity(data []byte) {
	binary.LittleEndian.PutUint32(data, enc.next)
	binary.LittleEndian.PutUint16(data[4:], TypeParity)
	enc.next = (enc.next + 1) % enc.paws
}
