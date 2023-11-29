package lsvd

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"os"

	"github.com/igrmk/treemap/v2"
	"github.com/pierrec/lz4/v4"
)

type ocBlock struct {
	lba          LBA
	flags        byte
	size, offset uint64
}

type ObjectCreator struct {
	cnt int

	offset uint64
	blocks []ocBlock

	buf    []byte
	header bytes.Buffer
	body   bytes.Buffer
}

func emptyBytes(b []byte) bool {
	return bytes.Equal(b, emptyBlock)
}

func (o *ObjectCreator) WriteExtent(firstBlock LBA, ext Extent) error {
	if o.buf == nil {
		o.buf = make([]byte, 2*BlockSize)
	}
	for i := 0; i < ext.Blocks(); i++ {
		lba := firstBlock + LBA(i)

		var flags byte

		if emptyBytes(ext.BlockView(i)) {
			o.cnt++

			o.blocks = append(o.blocks, ocBlock{
				lba:   lba,
				flags: 2,
			})
			continue
		}

		sz, err := lz4.CompressBlock(ext.BlockView(i), o.buf, nil)
		if err != nil {
			return err
		}

		body := ext.BlockView(i)

		if sz > 0 && sz < BlockSize {
			body = o.buf[:sz]
			flags = 1
		}

		_, err = o.body.Write(body)
		if err != nil {
			return err
		}

		o.cnt++

		o.blocks = append(o.blocks, ocBlock{
			lba:    lba,
			size:   uint64(len(body)),
			offset: o.offset,
			flags:  flags,
		})

		o.offset += uint64(len(body))
	}

	return nil
}

func (o *ObjectCreator) Reset() {
	o.blocks = nil
	o.cnt = 0
	o.offset = 0
	o.header.Reset()
	o.body.Reset()
}

func (o *ObjectCreator) Flush(path string, seg SegmentId, m *treemap.TreeMap[LBA, objPBA]) error {
	defer o.Reset()

	buf := make([]byte, 16)

	for _, blk := range o.blocks {
		lba := blk.lba

		sz := binary.PutUvarint(buf, uint64(lba))
		_, err := o.header.Write(buf[:sz])
		if err != nil {
			return err
		}

		err = o.header.WriteByte(blk.flags)
		if err != nil {
			return err
		}

		sz = binary.PutUvarint(buf, blk.size)
		_, err = o.header.Write(buf[:sz])
		if err != nil {
			return err
		}

		sz = binary.PutUvarint(buf, blk.offset)
		_, err = o.header.Write(buf[:sz])
		if err != nil {
			return err
		}
	}

	dataBegin := uint32(o.header.Len() + 8)

	for _, blk := range o.blocks {
		m.Set(blk.lba, objPBA{
			PBA: PBA{
				Segment: seg,
				Offset:  dataBegin + uint32(blk.offset),
			},
			Flags: blk.flags,
			Size:  uint32(blk.size),
		})

	}

	f, err := os.Create(path)
	if err != nil {
		return err
	}

	defer f.Close()

	binary.BigEndian.PutUint32(buf, uint32(o.cnt))
	_, err = f.Write(buf[:4])
	if err != nil {
		return err
	}

	binary.BigEndian.PutUint32(buf, dataBegin)
	_, err = f.Write(buf[:4])
	if err != nil {
		return err
	}

	_, err = io.Copy(f, &o.header)
	if err != nil {
		return err
	}

	_, err = io.Copy(f, &o.body)
	if err != nil {
		return err
	}

	return nil
}

type ObjectReader interface {
	io.ReaderAt
	io.Closer

	ReadAtCompressed(b []byte, off, compSize int64) (int, error)
}

type LocalFile struct {
	f *os.File
}

func (l *LocalFile) ReadAt(b []byte, off int64) (int, error) {
	return l.f.ReadAt(b, off)
}

func (l *LocalFile) ReadAtCompressed(dest []byte, off, compSize int64) (int, error) {
	buf := make([]byte, compSize)

	_, err := l.f.ReadAt(buf, off)
	if err != nil {
		return 0, err
	}

	sz, err := lz4.UncompressBlock(buf, dest)
	if err != nil {
		return 0, err
	}

	if sz != BlockSize {
		return 0, fmt.Errorf("compressed block uncompressed wrong size (%d != %d)", sz, BlockSize)
	}

	return len(dest), nil
}

func OpenLocalFile(path string) (*LocalFile, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}

	return &LocalFile{f: f}, nil
}

func (l *LocalFile) Close() error {
	return l.f.Close()
}
