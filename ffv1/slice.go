package ffv1

import (
	"fmt"
	"math"

	"github.com/dwbuiten/go-ffv1/ffv1/golomb"
	"github.com/dwbuiten/go-ffv1/ffv1/rangecoder"
)

type internalFrame struct {
	keyframe   bool
	slice_info []sliceInfo
	slices     []slice
}

type sliceInfo struct {
	pos          int
	size         uint32
	error_status uint8
}

type slice struct {
	header       sliceHeader
	start_x      uint32
	start_y      uint32
	width        uint32
	height       uint32
	state        [][][]uint8
	golomb_state [][]golomb.State
}

type sliceHeader struct {
	slice_width_minus1    uint32
	slice_height_minus1   uint32
	slice_x               uint32
	slice_y               uint32
	quant_table_set_index []uint8
	picture_structure     uint8
	sar_num               uint32
	sar_den               uint32
}

func countSlices(buf []byte, header *internalFrame, ec bool) error {
	footerSize := 3
	if ec {
		footerSize += 5
	}

	// Go over the packet from the end to start, reading the footer,
	// so we can derive the slice positions within the packet, and
	// allow multithreading.
	endPos := len(buf)
	header.slice_info = nil
	for endPos > 0 {
		var info sliceInfo

		size := uint32(buf[endPos-footerSize]) << 16
		size |= uint32(buf[endPos-footerSize+1]) << 8
		size |= uint32(buf[endPos-footerSize+2])

		info.size = size
		info.error_status = uint8(buf[endPos-footerSize+3])
		info.pos = endPos - int(size) - footerSize
		header.slice_info = append([]sliceInfo{info}, header.slice_info...) //prepend
		endPos = info.pos
	}

	if endPos < 0 {
		return fmt.Errorf("invalid slice footer")
	}

	return nil
}

func (d *Decoder) parseFooters(buf []byte, header *internalFrame) error {
	err := countSlices(buf, header, d.record.ec != 0)
	if err != nil {
		return fmt.Errorf("couldn't count slices: %s", err.Error())
	}

	slices := make([]slice, len(header.slice_info))
	if !header.keyframe {
		if len(slices) != len(header.slices) {
			return fmt.Errorf("inter frames must have the same number of slices as the preceding intra frame")
		}
		for i := 0; i < len(slices); i++ {
			slices[i].state = header.slices[i].state
		}
		if d.record.coder_type == 0 {
			for i := 0; i < len(slices); i++ {
				slices[i].golomb_state = header.slices[i].golomb_state
			}
		}
	}
	header.slices = slices

	return nil
}

func (d *Decoder) parseSliceHeader(c *rangecoder.Coder, s *slice) {
	slice_state := make([]uint8, contextSize)
	for i := 0; i < contextSize; i++ {
		slice_state[i] = 128
	}

	s.header.slice_x = c.UR(slice_state)
	s.header.slice_y = c.UR(slice_state)
	s.header.slice_width_minus1 = c.UR(slice_state)
	s.header.slice_height_minus1 = c.UR(slice_state)

	quant_table_set_index_count := 1
	if d.record.chroma_planes {
		quant_table_set_index_count++
	}
	if d.record.extra_plane {
		quant_table_set_index_count++
	}

	s.header.quant_table_set_index = make([]uint8, quant_table_set_index_count)

	for i := 0; i < quant_table_set_index_count; i++ {
		s.header.quant_table_set_index[i] = uint8(c.UR(slice_state))
	}

	s.header.picture_structure = uint8(c.UR(slice_state))
	s.header.sar_num = c.UR(slice_state)
	s.header.sar_den = c.UR(slice_state)

	// Calculate bounaries for easy use elsewhere
	s.start_x = s.header.slice_x * d.width / (uint32(d.record.num_h_slices_minus1) + 1)
	s.start_y = s.header.slice_y * d.height / (uint32(d.record.num_v_slices_minus1) + 1)
	s.width = ((s.header.slice_x + s.header.slice_width_minus1 + 1) * d.width / (uint32(d.record.num_h_slices_minus1) + 1)) - s.start_x
	s.height = ((s.header.slice_y + s.header.slice_height_minus1 + 1) * d.height / (uint32(d.record.num_v_slices_minus1) + 1)) - s.start_y
}

func (d *Decoder) decodeSliceContent(c *rangecoder.Coder, gc *golomb.Coder, si *sliceInfo, s *slice, frame *Frame) {
	primary_color_count := 1
	chroma_planes := 0
	if d.record.chroma_planes {
		chroma_planes = 2
		primary_color_count += 2
	}
	if d.record.extra_plane {
		primary_color_count++
	}

	for p := 0; p < primary_color_count; p++ {
		var plane_pixel_height int
		var plane_pixel_width int
		var plane_pixel_stride int
		var start_x int
		var start_y int
		var quant_table int
		if p == 0 || p == 1+chroma_planes {
			plane_pixel_height = int(s.height)
			plane_pixel_width = int(s.width)
			plane_pixel_stride = int(d.width)
			start_x = int(s.start_x)
			start_y = int(s.start_y)
			if p == 0 {
				quant_table = 0
			} else {
				quant_table = chroma_planes
			}
		} else {
			// This is, of course, silly, but I want to do it "by the spec".
			plane_pixel_height = int(math.Ceil(float64(s.height) / float64(uint32(1)<<d.record.log2_v_chroma_subsample)))
			plane_pixel_width = int(math.Ceil(float64(s.width) / float64(uint32(1)<<d.record.log2_h_chroma_subsample)))
			plane_pixel_stride = int(math.Ceil(float64(d.width) / float64(uint32(1)<<d.record.log2_h_chroma_subsample)))
			start_x = int(math.Ceil(float64(s.start_x) / float64(uint32(1)<<d.record.log2_v_chroma_subsample)))
			start_y = int(math.Ceil(float64(s.start_y) / float64(uint32(1)<<d.record.log2_h_chroma_subsample)))
			quant_table = 1
		}

		// 3.8.2.2.1. Run Length Coding
		if gc != nil {
			gc.NewPlane(uint32(plane_pixel_width))
		}

		for y := 0; y < plane_pixel_height; y++ {
			// Line()
			if gc != nil {
				gc.NewLine()
			}
			for x := 0; x < plane_pixel_width; x++ {
				var sign bool

				var buf []byte
				var buf16 []uint16
				if d.record.bits_per_raw_sample == 8 {
					buf = frame.Buf[p][start_y*plane_pixel_stride+start_x:]
				} else {
					buf16 = frame.Buf16[p][start_y*plane_pixel_stride+start_x:]
				}

				// Derive neighbours
				var T, L, t, l, tr, tl int
				if d.record.bits_per_raw_sample == 8 {
					T, L, t, l, tr, tl = deriveBorders(buf, x, y, plane_pixel_width, plane_pixel_height, plane_pixel_stride)
				} else {
					T, L, t, l, tr, tl = deriveBorders16(buf16, x, y, plane_pixel_width, plane_pixel_height, plane_pixel_stride)
				}

				context := getContext(d.record.quant_tables[s.header.quant_table_set_index[quant_table]], T, L, t, l, tr, tl)
				if context < 0 {
					context = -context
					sign = true
				} else {
					sign = false
				}

				var diff int32
				if gc != nil {
					diff = gc.SG(context, &s.golomb_state[quant_table][context])
				} else {
					diff = c.SR(s.state[quant_table][context])
				}

				if sign {
					diff = -diff
				}

				val := diff + int32(getMedian(l, t, l+t-tl))
				val = val & ((1 << d.record.bits_per_raw_sample) - 1) // Section 3.8

				if d.record.bits_per_raw_sample == 8 {
					buf[(y*plane_pixel_stride)+x] = byte(val)
				} else {
					buf16[(y*plane_pixel_stride)+x] = uint16(val)
				}
			}
		}
	}
}

func isKeyframe(buf []byte) bool {
	state := make([]uint8, contextSize)
	for i := 0; i < contextSize; i++ {
		state[i] = 128
	}

	c := rangecoder.NewCoder(buf)

	return c.BR(state)
}

func (d *Decoder) resetSliceStates(s *slice) {
	// Range coder states
	s.state = make([][][]uint8, len(d.initial_states))
	for i := 0; i < len(d.initial_states); i++ {
		s.state[i] = make([][]uint8, len(d.initial_states[i]))
		for j := 0; j < len(d.initial_states[i]); j++ {
			s.state[i][j] = make([]uint8, len(d.initial_states[i][j]))
			copy(s.state[i][j], d.initial_states[i][j])
		}
	}

	// Golomb-Rice Code states
	if d.record.coder_type == 0 {
		s.golomb_state = make([][]golomb.State, d.record.quant_table_set_count)
		for i := 0; i < len(s.golomb_state); i++ {
			s.golomb_state[i] = make([]golomb.State, d.record.context_count[i])
			for j := 0; j < len(s.golomb_state[i]); j++ {
				s.golomb_state[i][j] = golomb.NewState()
			}
		}
	}
}

func (d *Decoder) decodeSlice(buf []byte, header *internalFrame, slicenum int, frame *Frame) error {
	// Before we do anything, let's try and check the integrity
	if d.record.ec == 1 {
		if header.slice_info[slicenum].error_status != 0 {
			return fmt.Errorf("error_status is non-zero: %d", header.slice_info[slicenum].error_status)
		}

		sliceBuf := buf[header.slice_info[slicenum].pos:]
		sliceBuf = sliceBuf[:header.slice_info[slicenum].size+8] // 8 bytes for footer size
		if crc32MPEG2(sliceBuf) != 0 {
			return fmt.Errorf("CRC mismatch")
		}
	}

	// If this is a keyframe, refresh states.
	if header.keyframe {
		d.resetSliceStates(&header.slices[slicenum])
	}

	c := rangecoder.NewCoder(buf[header.slice_info[slicenum].pos:])

	state := make([]uint8, contextSize)
	for i := 0; i < contextSize; i++ {
		state[i] = 128
	}

	// Skip keyframe bit on slice 0
	if slicenum == 0 {
		c.BR(state)
	}

	if d.record.coder_type == 2 { // Custom state transition table
		c.SetTable(d.state_transition)
	}

	d.parseSliceHeader(c, &header.slices[slicenum])

	var gc *golomb.Coder
	if d.record.coder_type == 0 {
		c.SentinalEnd() // We're switching to Golomb-Rice mode now
		offset := c.GetPos() - 1
		gc = golomb.NewCoder(buf[header.slice_info[slicenum].pos+offset:])
	}

	d.decodeSliceContent(c, gc, &header.slice_info[slicenum], &header.slices[slicenum], frame)

	return nil
}
