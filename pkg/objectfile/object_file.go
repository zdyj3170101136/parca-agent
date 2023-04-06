// Copyright 2022-2023 The Parca Authors
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.
//

// This package includes modified code from the github.com/google/pprof/internal/binutils

package objectfile

import (
	"debug/elf"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/parca-dev/parca-agent/internal/pprof/elfexec"
	"github.com/parca-dev/parca-agent/pkg/buildid"
)

var (
	elfOpen    = elf.Open
	elfNewFile = elf.NewFile
)

type ObjectFile struct {
	// TODO: Move to Mapping.
	Pid     int

	Path    string
	BuildID string
	File    *os.File

	DebuginfoFile *os.File
	DebuginfoFileSize int64
	DebuginfoModTime  time.Time

	// Ensures the base, baseErr and isData are computed once.
	baseOnce sync.Once
	base     uint64
	baseErr  error

	isData bool
	m      *mapping
}

func rewindFile(f *os.File, err error) error {
	_, sErr := f.Seek(0, io.SeekStart)
	if sErr != nil {
		sErr = fmt.Errorf("failed to seek to the beginning of the file %s: %w", f.Name(), sErr)
		if err == nil {
			return sErr
		}
		return errors.Join(err, sErr)
	}
	return err
}

// Open opens the specified executable or library file from the given path.
func Open(filePath string, start, limit, offset uint64) (_ *ObjectFile, err error) {
	f, err := os.Open(filePath)
	if err != nil {
		return nil, fmt.Errorf("error opening %s: %w", filePath, err)
	}
	defer func() {
		err = rewindFile(f, err)
	}()
	// defer f.Close(): a problem for our future selves

	ok, err := isELF(f)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, fmt.Errorf("unrecognized binary format: %s", filePath)
	}

	relocationSymbol := kernelRelocationSymbol(f.Name()) // TODO: Should this just the file name?
	objFile, err := open(f, start, limit, offset, relocationSymbol)
	if err != nil {
		return nil, fmt.Errorf("error reading ELF file %s: %w", filePath, err)
	}

	return objFile, nil
}

// isELF opens a file to check whether its format is ELF.
func isELF(f *os.File) (_ bool, err error) {
	defer func() {
		err = rewindFile(f, err)
	}()

	// Read the first 4 bytes of the file.
	var header [4]byte
	if _, err := f.Read(header[:]); err != nil {
		return false, fmt.Errorf("error reading magic number from %s: %w", f.Name(), err)
	}

	// Match against supported file types.
	isELFMagic := string(header[:]) == elf.ELFMAG

	return isELFMagic, nil
}

// kernelRelocationSymbol extracts kernel relocation symbol _text or _stext
// for a main linux kernel mapping.
// The mapping file can be [kernel.kallsyms]_text or [kernel.kallsyms]_stext.
func kernelRelocationSymbol(mappingFile string) string {
	const prefix = "[kernel.kallsyms]"
	if !strings.HasPrefix(mappingFile, prefix) {
		return ""
	}

	return mappingFile[len(prefix):]
}

func (o *ObjectFile) Close() error {
	var err error
	if o != nil && o.File != nil {
		err = errors.Join(err, o.File.Close())
	}

	if o != nil && o.DebuginfoFile != nil {
		err = errors.Join(err, o.DebuginfoFile.Close())
	}

	return err
}

func open(f *os.File, start, limit, offset uint64, relocationSymbol string) (_ *ObjectFile, err error) {
	if f == nil {
		return nil, errors.New("nil file")
	}
	elfFile, err := elfNewFile(f)
	if err != nil {
		return nil, err
	}
	defer func() {
		err = rewindFile(f, err)
	}()

	filePath := f.Name()

	buildID := ""
	if id, err := buildid.BuildID(&buildid.ElfFile{Path: filePath, File: elfFile}); err == nil {
		buildID = id
	}

	var (
		kernelOffset *uint64
		pageAligned  = func(addr uint64) bool { return addr%4096 == 0 }
	)
	if strings.Contains(filePath, "vmlinux") || !pageAligned(start) || !pageAligned(limit) || !pageAligned(offset) {
		// Reading all Symbols is expensive, and we only rarely need it so
		// we don't want to do it every time. But if _stext happens to be
		// page-aligned but isn't the same as Vaddr, we would symbolize
		// wrong. So if the name the addresses aren't page aligned, or if
		// the name is "vmlinux" we read _stext. We can be wrong if: (1)
		// someone passes a kernel path that doesn't contain "vmlinux" AND
		// (2) _stext is page-aligned AND (3) _stext is not at Vaddr
		symbols, err := elfFile.Symbols()
		if err != nil && !errors.Is(err, elf.ErrNoSymbols) {
			return nil, err
		}

		// The kernel relocation symbol (the mapping start address) can be either
		// _text or _stext. When profiles are generated by `perf`, which one was used is
		// distinguished by the mapping name for the kernel image:
		// '[kernel.kallsyms]_text' or '[kernel.kallsyms]_stext', respectively. If we haven't
		// been able to parse it from the mapping, we default to _stext.
		if relocationSymbol == "" {
			relocationSymbol = "_stext"
		}
		for _, s := range symbols {
			sym := s
			if sym.Name == relocationSymbol {
				kernelOffset = &sym.Value
				break
			}
		}
	}

	// Check that we can compute a base for the binary. This may not be the
	// correct base value, so we don't save it. We delay computing the actual base
	// value until we have a sample address for this mapping, so that we can
	// correctly identify the associated program segment that is needed to compute
	// the base.
	if _, err := elfexec.GetBase(&elfFile.FileHeader, elfexec.FindTextProgHeader(elfFile), kernelOffset, start, limit, offset); err != nil {
		return nil, fmt.Errorf("could not identify base for %s: %w", filePath, err)
	}
	return &ObjectFile{
		Path:    filePath,
		BuildID: buildID,
		File:    f,
		m: &mapping{
			start:        start,
			limit:        limit,
			offset:       offset,
			kernelOffset: kernelOffset,
		},
	}, nil
}

func (f *ObjectFile) ObjAddr(addr uint64) (uint64, error) {
	f.baseOnce.Do(func() { f.baseErr = f.computeBase(addr) })
	if f.baseErr != nil {
		return 0, f.baseErr
	}
	return addr - f.base, nil
}

// computeBase computes the relocation base for the given binary ObjectFile only if
// the mapping field is set. It populates the base and isData fields and
// returns an error.
func (f *ObjectFile) computeBase(addr uint64) (err error) {
	if f == nil || f.m == nil {
		return nil
	}
	if addr < f.m.start || addr >= f.m.limit {
		return fmt.Errorf("specified address %x is outside the mapping range [%x, %x] for ObjectFile %q", addr, f.m.start, f.m.limit, f.Path)
	}

	ef, err := elfNewFile(f.File)
	if err != nil {
		return err
	}

	defer func() {
		err = rewindFile(f.File, err)
	}()

	ph, err := f.m.findProgramHeader(ef, addr)
	if err != nil {
		return fmt.Errorf("failed to find program header for ObjectFile %q, ELF mapping %#v, address %x: %w", f.Path, *f.m, addr, err)
	}

	base, err := elfexec.GetBase(&ef.FileHeader, ph, f.m.kernelOffset, f.m.start, f.m.limit, f.m.offset)
	if err != nil {
		return err
	}
	f.base = base
	f.isData = ph != nil && ph.Flags&elf.PF_X == 0
	return nil
}

type mapping struct {
	// Runtime mapping parameters.
	start, limit, offset uint64
	// Offset of kernel relocation symbol. Only defined for kernel images, nil otherwise. e. g. _stext.
	kernelOffset *uint64
}

// findProgramHeader returns the program segment that matches the current
// mapping and the given address, or an error if it cannot find a unique program
// header.
func (m *mapping) findProgramHeader(ef *elf.File, addr uint64) (*elf.ProgHeader, error) {
	// For user space executables, we try to find the actual program segment that
	// is associated with the given mapping. Skip this search if limit <= start.
	// We cannot use just a check on the start address of the mapping to tell if
	// it's a kernel / .ko module mapping, because with quipper address remapping
	// enabled, the address would be in the lower half of the address space.

	if m.kernelOffset != nil || m.start >= m.limit || m.limit >= (uint64(1)<<63) {
		// For the kernel, find the program segment that includes the .text section.
		return elfexec.FindTextProgHeader(ef), nil
	}

	// Fetch all the loadable segments.
	var phdrs []elf.ProgHeader
	for i := range ef.Progs {
		if ef.Progs[i].Type == elf.PT_LOAD {
			phdrs = append(phdrs, ef.Progs[i].ProgHeader)
		}
	}
	// Some ELF files don't contain any loadable program segments, e.g. .ko
	// kernel modules. It's not an error to have no header in such cases.
	if len(phdrs) == 0 {
		//nolint:nilnil
		return nil, nil
	}
	// Get all program headers associated with the mapping.
	headers := elfexec.ProgramHeadersForMapping(phdrs, m.offset, m.limit-m.start)
	if len(headers) == 0 {
		return nil, errors.New("no program header matches mapping info")
	}
	if len(headers) == 1 {
		return headers[0], nil
	}

	// Use the file offset corresponding to the address to symbolize, to narrow
	// down the header.
	return elfexec.HeaderForFileOffset(headers, addr-m.start+m.offset)
}
