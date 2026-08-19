package agc

import (
	"fmt"
	"io"
)

// ContigReader reads one sample's contigs in archive order. A reader is not
// safe for concurrent use, although independent readers may share an Archive.
type ContigReader struct {
	archive *Archive
	sample  Sample
	next    int
	count   int
}

// NewContigReader returns a pull-style reader for one sample. Read holds at
// most one returned contig's decoded sequence at a time.
func (a *Archive) NewContigReader(sample Sample) (*ContigReader, error) {
	count, err := a.contigCount(sample)
	if err != nil {
		return nil, err
	}
	return &ContigReader{archive: a, sample: sample, count: count}, nil
}

// Read decodes and returns the next contig, or io.EOF after the sample's final
// contig.
func (r *ContigReader) Read() (Contig, error) {
	if r.next >= r.count {
		return Contig{}, io.EOF
	}
	contig, err := r.archive.contigAt(r.sample, r.next)
	if err != nil {
		return Contig{}, err
	}
	r.next++
	return contig, nil
}

// IterateSample decodes a sample's contigs in archive order, holding at most
// one yielded contig's sequence at a time. Iteration stops on the first error
// returned by yield.
func (a *Archive) IterateSample(sample Sample, yield func(Contig) error) error {
	if yield == nil {
		return fmt.Errorf("agc: nil IterateSample callback")
	}
	r, err := a.NewContigReader(sample)
	if err != nil {
		return err
	}
	for {
		contig, err := r.Read()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		if err := yield(contig); err != nil {
			return err
		}
	}
}

// IterateAll decodes every contig in archive sample order and contig order,
// stopping on the first error returned by yield.
func (a *Archive) IterateAll(yield func(Contig) error) error {
	if yield == nil {
		return fmt.Errorf("agc: nil IterateAll callback")
	}
	samples, err := a.Samples()
	if err != nil {
		return err
	}
	for _, sample := range samples {
		if err := a.IterateSample(sample, yield); err != nil {
			return err
		}
	}
	return nil
}
