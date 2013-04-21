// RDiff clone.
//
// A replacement for the aging http://librsync.sourcefrog.net
// rdiff utility.
package main

import (
	"bitbucket.org/kardianos/rsync"

	"bytes"
	"crypto/md5"
	"encoding/gob"
	"errors"
	"flag"
	"fmt"
	"hash"
	"io"
	"log"
	"os"
	"strings"
)

var NoTargetSumError = errors.New("Checksum request but missing target hash.")
var HashNoMatchError = errors.New("Final data hash does not match.")

var fl = flag.NewFlagSet("rdiff", flag.ContinueOnError)

var blockSizeKiB = fl.Int("block", 6, "Block size in KiB")
var checkFile = fl.Bool("check", true, "Verify file with checksum")

func main() {
	var err error
	err = fl.Parse(os.Args[1:])
	if err != nil {
		printHelp()
		os.Exit(1)
	}

	var verb = strings.ToLower(fl.Arg(0))
	if len(verb) == 0 {
		log.Printf("Error: Must provide a verb.")
		printHelp()
		os.Exit(1)
	}

	if *blockSizeKiB <= 0 {
		log.Printf("Error: Invalid block size.")
		printHelp()
		os.Exit(1)
	}

	switch verb {
	case "signature":
		err = signature(fl.Arg(1), fl.Arg(2))
	case "delta":
		err = delta(fl.Arg(1), fl.Arg(2), fl.Arg(3))
	case "patch":
		err = patch(fl.Arg(1), fl.Arg(2), fl.Arg(3))
	case "test":
		err = test(fl.Arg(1), fl.Arg(2))
	default:
		log.Printf("Error: Unrecognized verb: %s", verb)
		printHelp()
		os.Exit(1)
	}
	if err != nil {
		log.Printf("Error in %s: %s", verb, err)
		os.Exit(2)
	}
}
func printHelp() {
	fmt.Printf(`
%s [options] signature BASIS SIGNATURE
%s [options] delta SIGNATURE NEWFILE DELTA
%s [options] patch BASIS DELTA NEWFILE
%s [options] test BASIS BASISv2
`, os.Args[0], os.Args[0], os.Args[0], os.Args[0])
	fl.PrintDefaults()
}

func getRsync() *rsync.RSync {
	return &rsync.RSync{
		BlockSize: 1024 * *blockSizeKiB,
		MaxDataOp: 1024 * 1024,
	}
}

func signature(basis, signature string) error {
	rs := getRsync()
	basisFile, err := os.Open(basis)
	if err != nil {
		return err
	}
	defer basisFile.Close()

	sigFile, err := os.Create(signature)
	if err != nil {
		return err
	}
	defer sigFile.Close()

	sigEncode := gob.NewEncoder(sigFile)
	err = sigEncode.Encode(rs.BlockSize)
	if err != nil {
		return err
	}
	return rs.CreateSignature(basisFile, func(block rsync.BlockHash) error {
		// Save signature hash list to file.
		return sigEncode.Encode(block)
	})
}

func delta(signature, newfile, delta string) error {
	rs := getRsync()
	sigFile, err := os.Open(signature)
	if err != nil {
		return err
	}
	defer sigFile.Close()

	nfFile, err := os.Open(newfile)
	if err != nil {
		return err
	}
	defer nfFile.Close()

	deltaFile, err := os.Create(delta)
	if err != nil {
		return err
	}
	defer deltaFile.Close()

	// Load signature hash list.
	hl := make([]rsync.BlockHash, 0)
	sigDecode := gob.NewDecoder(sigFile)
	err = sigDecode.Decode(&rs.BlockSize)
	if err != nil {
		if err == io.EOF {
			return io.ErrUnexpectedEOF
		}
		return err
	}
	for {
		bl := rsync.BlockHash{}
		err = sigDecode.Decode(&bl)
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		hl = append(hl, bl)
	}

	// Save operations to file.
	opsEncode := gob.NewEncoder(deltaFile)
	err = opsEncode.Encode(rs.BlockSize)
	if err != nil {
		return err
	}

	var hasher hash.Hash
	if *checkFile {
		hasher = md5.New()
	}
	err = rs.CreateDelta(nfFile, hl, func(op rsync.Operation) error {
		return opsEncode.Encode(op)
	}, hasher)
	if err != nil {
		return err
	}
	if *checkFile {
		return opsEncode.Encode(rsync.Operation{
			Type: rsync.HASH,
			Data: hasher.Sum(nil),
		})
	}
	return nil
}

func patch(basis, delta, newfile string) error {
	rs := getRsync()
	basisFile, err := os.Open(basis)
	if err != nil {
		return err
	}
	defer basisFile.Close()

	deltaFile, err := os.Open(delta)
	if err != nil {
		return err
	}
	defer deltaFile.Close()

	fsFile, err := os.Create(newfile)
	if err != nil {
		return err
	}
	defer fsFile.Close()

	var sourceSum []byte
	deltaDecode := gob.NewDecoder(deltaFile)
	err = deltaDecode.Decode(&rs.BlockSize)
	if err != nil {
		if err == io.EOF {
			return io.ErrUnexpectedEOF
		}
		return err
	}

	ops := make(chan rsync.Operation)
	// Load operations from file.
	var decodeError error
	go func() {
		defer close(ops)
		for {
			op := rsync.Operation{}
			err = deltaDecode.Decode(&op)
			if err == io.EOF {
				break
			}
			if err != nil {
				decodeError = err
				return
			}
			if op.Type == rsync.HASH {
				sourceSum = op.Data
				continue
			}
			ops <- op
		}
	}()

	var hasher hash.Hash
	if *checkFile {
		hasher = md5.New()
	}
	err = rs.ApplyDelta(fsFile, basisFile, ops, hasher)
	if err != nil {
		return err
	}
	if decodeError != nil {
		return decodeError
	}
	if *checkFile == false {
		return nil
	}
	if sourceSum == nil {
		return NoTargetSumError
	}
	if bytes.Equal(sourceSum, hasher.Sum(nil)) == false {
		return HashNoMatchError
	}

	return nil
}

func test(basis1, basis2 string) error {
	basis1File, err := os.Open(basis1)
	if err != nil {
		return err
	}
	defer basis1File.Close()

	basis2File, err := os.Open(basis2)
	if err != nil {
		return err
	}
	defer basis2File.Close()

	basis1Stat, err := basis1File.Stat()
	if err != nil {
		return err
	}
	basis2Stat, err := basis2File.Stat()
	if err != nil {
		return err
	}

	if basis1Stat.Size() != basis2Stat.Size() {
		return fmt.Errorf("File size different.")
	}

	type resetBuffer struct {
		orig, buf []byte
	}

	bufferFount := make(chan resetBuffer, 30)

	b1Source := make(chan resetBuffer, 10)
	b2Source := make(chan resetBuffer, 10)
	errorSource := make(chan error, 4)

	for i := 0; i < cap(bufferFount); i++ {
		b := make([]byte, 32*1024)

		bufferFount <- resetBuffer{
			orig: b,
			buf:  b,
		}
	}

	reader := func(f io.Reader, source chan resetBuffer, errorSource chan error) {
		for {
			buffer := <-bufferFount
			buffer.buf = buffer.orig
			n, err := f.Read(buffer.orig)
			if n == 0 {
				bufferFount <- buffer
			} else {
				buffer.buf = buffer.orig[:n]
				source <- buffer
			}
			if err != nil {
				if err == io.EOF {
					close(source)
					return
				}
				errorSource <- fmt.Errorf("Error reading file: %s", err)
				return
			}
		}
	}

	go reader(basis1File, b1Source, errorSource)
	go reader(basis2File, b2Source, errorSource)

	location := 0
	var b1Buffer resetBuffer
	var b2Buffer resetBuffer
	var ok bool
	for {
		if len(errorSource) > 0 {
			return <-errorSource
		}
		if len(b1Buffer.buf) == 0 {
			if b1Buffer.buf != nil {
				bufferFount <- b1Buffer
			}
			b1Buffer, ok = <-b1Source
			if !ok {
				return nil
			}
		}
		if len(b2Buffer.buf) == 0 {
			if b2Buffer.buf != nil {
				bufferFount <- b2Buffer
			}
			b2Buffer, ok = <-b2Source
			if !ok {
				return nil
			}
		}
		size := min(len(b1Buffer.buf), len(b2Buffer.buf))

		for i := 0; i < size; i++ {
			if b1Buffer.buf[i] != b2Buffer.buf[i] {
				return fmt.Errorf("FAIL: Bytes differ at %d.", location)
			}
			location++
		}
		b1Buffer.buf = b1Buffer.buf[size:]
		b2Buffer.buf = b2Buffer.buf[size:]
	}
	return nil
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}