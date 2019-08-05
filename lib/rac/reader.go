// Copyright 2019 The Wuffs Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//    https://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package rac

import (
	"errors"
	"hash/crc32"
	"io"
)

var (
	errInvalidIndexNode = errors.New("rac: invalid index node")
)

func u48LE(b []byte) int64 {
	_ = b[7] // Early bounds check to guarantee safety of reads below.
	u := uint64(b[0]) | uint64(b[1])<<8 | uint64(b[2])<<16 | uint64(b[3])<<24 |
		uint64(b[4])<<32 | uint64(b[5])<<40 | uint64(b[6])<<48 | uint64(b[7])<<56
	return int64(u & 0xFFFFFFFFFFFF)
}

// readAt calls ReadAt if it is available, otherwise it falls back to calling
// Seek and then ReadFull. Calling ReadAt is presumably slightly more
// efficient, e.g. one syscall instead of two.
func readAt(r io.ReadSeeker, p []byte, offset int64) error {
	if a, ok := r.(io.ReaderAt); ok && false {
		n, err := a.ReadAt(p, offset)
		if (n == len(p)) && (err == io.EOF) {
			err = nil
		}
		return err
	}
	if _, err := r.Seek(offset, io.SeekStart); err != nil {
		return err
	}
	_, err := io.ReadFull(r, p)
	return err
}

// bitwiseSubset returns whether every 1-bit in inner is also set in outer.
func bitwiseSubset(outer Codec, inner Codec) bool {
	return outer == (outer | inner)
}

// Range is the half-open range [low, high). It is invalid for low to be
// greater than high.
type Range [2]int64

func (r *Range) Empty() bool { return r[0] == r[1] }
func (r *Range) Size() int64 { return r[1] - r[0] }

// Chunk is a compressed chunk returned by a Reader.
//
// See the RAC specification for further discussion.
type Chunk struct {
	DRange     Range
	CPrimary   Range
	CSecondary Range
	CTertiary  Range
	STag       uint8
	TTag       uint8
	Codec      Codec
}

// nodeSize returns the size (in CSpace) that a node with the given arity
// occupies.
func nodeSize(arity uint8) int {
	return (16 * int(arity)) + 16
}

// rNode is the Reader's representation of a node.
//
// None of its methods, other than valid, should be called unless valid returns
// true.
type rNode [4096]byte

func (b *rNode) arity() int     { return int(b[3]) }
func (b *rNode) codec() Codec   { return Codec(b[(8*int(b[3]))+7]) }
func (b *rNode) cPtrMax() int64 { return u48LE(b[(16*int(b[3]))+8:]) }
func (b *rNode) dPtrMax() int64 { return u48LE(b[8*int(b[3]):]) }
func (b *rNode) version() uint8 { return b[(16*int(b[3]))+14] }

func (b *rNode) cLen(i int) uint8 {
	base := (8 * int(b[3])) + 14
	return b[(8*i)+base]
}

func (b *rNode) cOff(i int, cBias int64) int64 {
	base := (8 * int(b[3])) + 8
	return cBias + u48LE(b[(8*i)+base:])
}

func (b *rNode) cOffRange(i int, cBias int64) Range {
	m := cBias + b.cPtrMax()
	if i >= b.arity() {
		return Range{m, m}
	}
	cOff := b.cOff(i, cBias)
	if cLen := b.cLen(i); cLen != 0 {
		if n := cOff + (int64(cLen) * 1024); m > n {
			m = n
		}
	}
	return Range{cOff, m}
}

func (b *rNode) dOff(i int, dBias int64) int64 {
	if i == 0 {
		return dBias
	}
	return dBias + u48LE(b[8*i:])
}

func (b *rNode) dOffRange(i int, dBias int64) Range {
	return Range{b.dOff(i, dBias), b.dOff(i+1, dBias)}
}

func (b *rNode) dSize(i int) int64 {
	x := int64(0)
	if i > 0 {
		x = u48LE(b[8*i:])
	}
	return u48LE(b[(8*i)+8:]) - x
}

func (b *rNode) sTag(i int) uint8 {
	base := (8 * int(b[3])) + 15
	return b[(8*i)+base]
}

func (b *rNode) tTag(i int) uint8 {
	return b[(8*i)+7]
}

func (b *rNode) isLeaf(i int) bool {
	return b[(8*i)+7] != 0xFE
}

func (b *rNode) findChunkContaining(dOff int64, dBias int64) int {
	// TODO: binary search instead of linear search.
	for i, n := 0, b.arity(); i < n; i++ {
		if dOff < b.dOff(i+1, dBias) {
			return i
		}
	}
	// We shouldn't get here, since we validate each node, and don't call this
	// function for a DOff greater or equal to DOffMax.
	panic("rac: internal error: could not find containing chunk")
}

func (b *rNode) chunk(i int, cBias int64, dBias int64) Chunk {
	sTag := b.sTag(i)
	tTag := b.tTag(i)
	return Chunk{
		DRange:     b.dOffRange(i, dBias),
		CPrimary:   b.cOffRange(i, cBias),
		CSecondary: b.cOffRange(int(sTag), cBias),
		CTertiary:  b.cOffRange(int(tTag), cBias),
		STag:       sTag,
		TTag:       tTag,
		Codec:      b.codec(),
	}
}

func (b *rNode) valid() bool {
	// Check the magic and arity.
	if (b[0] != magic[0]) || (b[1] != magic[1]) || (b[2] != magic[2]) || (b[3] == 0) {
		return false
	}
	arity := int(b[3])
	size := (16 * arity) + 16
	if b[3] != b[size-1] {
		return false
	}

	// Check that the "Reserved (0)" bytes are zero and that the TTag values
	// aren't in the reserved range [0xC0, 0xFE).
	for i := 0; i < arity; i++ {
		if b[(8*i)+6] != 0 {
			return false
		}
		if tTag := b[(8*i)+7]; (0xC0 <= tTag) && (tTag < 0xFE) {
			return false
		}
	}
	if b[(8*arity)+6] != 0 {
		return false
	}

	// Check that the Codec is non-zero.
	if b[(8*arity)+7] == 0 {
		return false
	}

	// Check that the DPtr values are non-decreasing. The first DPtr value is
	// implicitly zero.
	prev := u48LE(b[8*1:])
	for i := 2; i <= arity; i++ {
		curr := u48LE(b[8*i:])
		if curr < prev {
			return false
		}
		prev = curr
	}

	// Check that no CPtr value exceeds CPtrMax (the final CPtr value).
	base := (8 * arity) + 8
	cPtrMax := u48LE(b[size-8:])
	for i := 0; i < arity; i++ {
		if cPtr := u48LE(b[(8*i)+base:]); cPtr > cPtrMax {
			return false
		}
	}

	// Check the the version is non-zero.
	if b[(16*arity)+14] == 0 {
		return false
	}

	// Check the checksum.
	checksum := crc32.ChecksumIEEE(b[6:size])
	checksum ^= checksum >> 16
	if (b[4] != uint8(checksum>>0)) || (b[5] != uint8(checksum>>8)) {
		return false
	}

	// Further checking of the codec, version, COffMax and DOffMax requires
	// more context, and is done in loadAndValidate.
	return true
}

// Reader reads a RAC file.
//
// Do not modify its exported fields after calling any of its methods.
type Reader struct {
	// ReadSeeker is where the RAC-encoded data is read from.
	//
	// It may also implement io.ReaderAt, in which case its ReadAt method will
	// be preferred over combining Read and Seek, as the former is presumably
	// more efficient. This is optional: io.ReaderAt is a stronger contract
	// than io.ReadSeeker, as multiple concurrent ReadAt calls must not
	// interfere with each other.
	//
	// For example, this type itself only implements io.ReadSeeker, not
	// io.ReaderAt, as it is not safe for concurrent use.
	//
	// Nil is an invalid value.
	ReadSeeker io.ReadSeeker

	// CompressedSize is the size of the RAC file.
	//
	// Zero is an invalid value, as an empty file is not a valid RAC file.
	CompressedSize int64

	// initialized is set true after the first call on this Reader.
	initialized bool

	// rootNodeArity is the root node's arity.
	rootNodeArity uint8

	// needToResolveSeekPosition is whether NextChunk will need to resolve
	// seekPosition.
	needToResolveSeekPosition bool

	// err is the first error encountered. It is sticky: once a non-nil error
	// occurs, all public methods will return that error.
	err error

	// decompressedSize is the size of the RAC file in DSpace.
	decompressedSize int64

	// rootNodeCOffset is the position of the root node in the RAC file.
	rootNodeCOffset int64

	// seekPosition gives, if needToResolveSeekPosition is true, the position
	// in DSpace that NextChunk needs to find.
	seekPosition int64

	// The i (as in "the i'th child of currNode") that denotes the next chunk
	// to be returned by NextChunk, if a non-empty chunk. If nextChunk equals
	// currNode's arity, then currNode is exhausted and calling NextChunk will
	// seek to the next node.
	nextChunk int32

	// The CBias and DBias of currNode.
	currNodeCBias int64
	currNodeDBias int64

	// currNode is the 4096 byte buffer to hold the current node.
	currNode rNode
}

func (r *Reader) checkParameters() error {
	if r.ReadSeeker == nil {
		r.err = errors.New("rac: invalid ReadSeeker")
		return r.err
	}
	if r.CompressedSize < 32 {
		r.err = errors.New("rac: invalid CompressedSize")
		return r.err
	}
	return nil
}

func (r *Reader) initialize() error {
	if r.err != nil {
		return r.err
	}
	if r.initialized {
		return nil
	}
	r.initialized = true

	if err := r.checkParameters(); err != nil {
		return err
	}

	if err := r.findRootNode(); err != nil {
		return err
	}
	if r.currNode.version() != 1 {
		r.err = errors.New("rac: unsupported RAC file version")
		return r.err
	}
	return nil
}

func (r *Reader) findRootNode() error {
	// Look at the start of the compressed file.
	if err := readAt(r.ReadSeeker, r.currNode[:4], 0); err != nil {
		r.err = err
		return r.err
	}
	if (r.currNode[0] != magic[0]) ||
		(r.currNode[1] != magic[1]) ||
		(r.currNode[2] != magic[2]) {
		r.err = errors.New("rac: invalid input: missing magic bytes at the start of file")
		return r.err
	}
	if found, err := r.tryRootNode(r.currNode[3], false); err != nil {
		return err
	} else if found {
		return nil
	}

	// Look at the end of the compressed file.
	if err := readAt(r.ReadSeeker, r.currNode[:1], r.CompressedSize-1); err != nil {
		r.err = err
		return r.err
	}
	if found, err := r.tryRootNode(r.currNode[0], true); err != nil {
		return err
	} else if found {
		return nil
	}

	return errors.New("rac: invalid input: missing index root node")
}

func (r *Reader) tryRootNode(arity uint8, fromEnd bool) (found bool, ioErr error) {
	if arity == 0 {
		return false, nil
	}
	size := int64(nodeSize(arity))
	if r.CompressedSize < size {
		return false, nil
	}
	cOffset := int64(0)
	if fromEnd {
		cOffset = r.CompressedSize - size
	}
	if err := r.load(cOffset, arity); err != nil {
		return false, err
	}
	if !r.currNode.valid() {
		return false, nil
	}
	if r.currNode.cPtrMax() != r.CompressedSize {
		return false, nil
	}
	r.needToResolveSeekPosition = true
	r.rootNodeCOffset = cOffset
	r.rootNodeArity = arity
	r.decompressedSize = r.currNode.dPtrMax()
	return true, nil
}

// load loads a node from the RAC file into r.currNode. It does not check that
// the result is valid, and the caller should do so if it doesn't already know
// that it is valid.
func (r *Reader) load(cOffset int64, arity uint8) error {
	if arity == 0 {
		r.err = errors.New("rac: internal error: inconsistent arity")
		return r.err
	}
	size := nodeSize(arity)
	if err := readAt(r.ReadSeeker, r.currNode[:size], cOffset); err != nil {
		r.err = err
		return r.err
	}
	return nil
}

func (r *Reader) loadAndValidate(cOffset int64,
	parentCodec Codec, parentVersion uint8, parentCOffMax int64,
	childCBias int64, childDSize int64) error {

	if (cOffset < 0) || ((r.CompressedSize - 4) < cOffset) {
		r.err = errInvalidIndexNode
		return r.err
	}
	if err := readAt(r.ReadSeeker, r.currNode[:4], cOffset); err != nil {
		r.err = err
		return r.err
	}
	arity := r.currNode[3]
	if arity == 0 {
		r.err = errInvalidIndexNode
		return r.err
	}
	size := int64(nodeSize(arity))
	if (r.CompressedSize < size) || ((r.CompressedSize - size) < cOffset) {
		r.err = errInvalidIndexNode
		return r.err
	}
	if err := r.load(cOffset, arity); err != nil {
		return err
	}

	if !r.currNode.valid() {
		r.err = errInvalidIndexNode
		return r.err
	}

	// Validate the parent and child codec, version, COffMax and DOffMax.
	childVersion := r.currNode.version()
	if !bitwiseSubset(parentCodec, r.currNode.codec()) ||
		(parentVersion < childVersion) ||
		(parentCOffMax < (childCBias + r.currNode.cPtrMax())) ||
		(childDSize != r.currNode.dPtrMax()) {
		r.err = errInvalidIndexNode
		return r.err
	}
	return nil
}

// DecompressedSize returns the total size of the decompressed data.
func (r *Reader) DecompressedSize() (int64, error) {
	if err := r.initialize(); err != nil {
		return 0, err
	}
	return r.decompressedSize, nil
}

// SeekToChunkContaining sets up NextChunk to return the chunk containing
// dSpaceOffset. That chunk does not necessarily start at dSpaceOffset.
//
// It is an error to seek to a negative value.
func (r *Reader) SeekToChunkContaining(dSpaceOffset int64) error {
	if err := r.initialize(); err != nil {
		return err
	}
	if dSpaceOffset < 0 {
		r.err = errors.New("rac: seek: negative position")
		return r.err
	}
	r.needToResolveSeekPosition = true
	r.seekPosition = dSpaceOffset
	return nil
}

// NextChunk returns the next independently compressed chunk, or io.EOF if
// there are no more chunks.
//
// Empty chunks (those that contain no decompressed data, only metadata) are
// skipped.
func (r *Reader) NextChunk() (Chunk, error) {
	if err := r.initialize(); err != nil {
		return Chunk{}, err
	}
	for {
		if r.needToResolveSeekPosition {
			if r.seekPosition >= r.decompressedSize {
				return Chunk{}, io.EOF
			}
			r.needToResolveSeekPosition = false
			if err := r.resolveSeekPosition(); err != nil {
				return Chunk{}, err
			}
		}
		for n := int32(r.currNode.arity()); r.nextChunk < n; {
			c := r.currNode.chunk(int(r.nextChunk), r.currNodeCBias, r.currNodeDBias)
			r.nextChunk++
			r.seekPosition = c.DRange[1]
			if !c.DRange.Empty() {
				return c, nil
			}
		}
		r.needToResolveSeekPosition = true
	}
}

func (r *Reader) resolveSeekPosition() error {
	// Load the root node. It has already been validated, during initialize.
	if err := r.load(r.rootNodeCOffset, r.rootNodeArity); err != nil {
		return err
	}

	// Walk the branch nodes until we find the leaf node containing the
	// seekPosition.
	cBias := int64(0)
	dBias := int64(0)
	for {
		i := r.currNode.findChunkContaining(r.seekPosition, dBias)
		if r.currNode.isLeaf(i) {
			r.nextChunk = int32(i)
			r.currNodeCBias = cBias
			r.currNodeDBias = dBias
			return nil
		}

		parentCodec := r.currNode.codec()
		parentVersion := r.currNode.version()
		parentCOffMax := cBias + r.currNode.cPtrMax()
		childCOffset := r.currNode.cOff(i, cBias)
		childCBias := cBias
		if sTag := int(r.currNode.sTag(i)); sTag < r.currNode.arity() {
			childCBias = r.currNode.cOff(sTag, cBias)
		}
		childDBias := r.currNode.dOff(i, dBias)
		childDSize := r.currNode.dSize(i)

		if err := r.loadAndValidate(childCOffset,
			parentCodec, parentVersion, parentCOffMax,
			childCBias, childDSize); err != nil {
			return err
		}

		cBias = childCBias
		dBias = childDBias
	}
}