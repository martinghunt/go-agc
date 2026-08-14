package agc

import "fmt"

// IterateSample decodes a sample's contigs in archive order, holding at most
// one yielded contig's sequence at a time. Iteration stops on the first error
// returned by yield.
func (a *Archive) IterateSample(sample Sample, yield func(Contig) error) error {
	if yield == nil {
		return fmt.Errorf("agc: nil IterateSample callback")
	}
	contigs, err := a.Contigs(sample)
	if err != nil {
		return err
	}
	for i := range contigs {
		contig, err := a.contigAt(sample, i)
		if err != nil {
			return err
		}
		if err := yield(contig); err != nil {
			return err
		}
	}
	return nil
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
