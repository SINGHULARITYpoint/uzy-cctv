package devices

import "fmt"

type h264SPSInfo struct {
	ProfileIDC    uint8
	LevelIDC      uint8
	Width         int
	Height        int
	HasDimensions bool
}

// countAnnexBNALTypes records NAL unit types in the Annex B output from
// pion's H264 depacketizer. The counts distinguish absent camera parameter
// sets from parameter sets lost during RTP frame assembly.
func countAnnexBNALTypes(data []byte, counts map[uint8]uint64) {
	for i := 0; i+4 < len(data); i++ {
		if data[i] != 0 || data[i+1] != 0 {
			continue
		}
		start := 0
		if data[i+2] == 1 {
			start = i + 3
		} else if i+4 < len(data) && data[i+2] == 0 && data[i+3] == 1 {
			start = i + 4
		}
		if start > 0 && start < len(data) {
			counts[data[start]&0x1f]++
			i = start
		}
	}
}

type h264BitReader struct {
	data []byte
	bit  int
}

func (r *h264BitReader) readBit() (uint32, bool) {
	if r.bit >= len(r.data)*8 {
		return 0, false
	}
	byteIdx := r.bit >> 3
	bitIdx := 7 - (r.bit & 7)
	r.bit++
	return uint32((r.data[byteIdx] >> bitIdx) & 1), true
}

func (r *h264BitReader) readBits(n int) (uint32, bool) {
	var val uint32
	for i := 0; i < n; i++ {
		b, ok := r.readBit()
		if !ok {
			return 0, false
		}
		val = (val << 1) | b
	}
	return val, true
}

func (r *h264BitReader) readUE() (uint32, bool) {
	zeros := 0
	for {
		b, ok := r.readBit()
		if !ok {
			return 0, false
		}
		if b == 1 {
			break
		}
		zeros++
		if zeros > 31 {
			return 0, false
		}
	}
	if zeros == 0 {
		return 0, true
	}
	suffix, ok := r.readBits(zeros)
	if !ok {
		return 0, false
	}
	return (1 << zeros) - 1 + suffix, true
}

func (r *h264BitReader) readSE() (int32, bool) {
	u, ok := r.readUE()
	if !ok {
		return 0, false
	}
	if u&1 != 0 {
		return int32((u + 1) >> 1), true
	}
	return -int32(u >> 1), true
}

func unescapeRBSP(nalu []byte) []byte {
	rbsp := make([]byte, 0, len(nalu))
	zeros := 0
	for _, b := range nalu {
		if zeros == 2 && b == 0x03 {
			zeros = 0
			continue
		}
		rbsp = append(rbsp, b)
		if b == 0 {
			zeros++
		} else {
			zeros = 0
		}
	}
	return rbsp
}

// parseH264SPS extracts profile, level, and cropped dimensions from a raw SPS NALU.
// Minimal synthetic 4-byte test stubs (0x67 profile constraints level) return
// HasDimensions=false with ProfileIDC and LevelIDC populated.
func parseH264SPS(nalu []byte) (h264SPSInfo, bool) {
	if len(nalu) < 4 || (nalu[0]&0x1f) != 7 {
		return h264SPSInfo{}, false
	}
	info := h264SPSInfo{
		ProfileIDC: nalu[1],
		LevelIDC:   nalu[3],
	}
	rbsp := unescapeRBSP(nalu[1:])
	if len(rbsp) < 4 {
		return info, true
	}
	r := h264BitReader{data: rbsp}
	profileIDC, ok := r.readBits(8)
	if !ok {
		return info, true
	}
	if _, ok := r.readBits(16); !ok { // constraint flags + level_idc
		return info, true
	}
	if _, ok := r.readUE(); !ok { // seq_parameter_set_id
		return info, true
	}
	chromaFormatIDC := uint32(1)
	separateColourPlane := uint32(0)
	switch profileIDC {
	case 100, 110, 122, 244, 44, 83, 86, 118, 128, 138, 139, 134, 135:
		var ok bool
		if chromaFormatIDC, ok = r.readUE(); !ok {
			return info, true
		}
		if chromaFormatIDC == 3 {
			if separateColourPlane, ok = r.readBit(); !ok {
				return info, true
			}
		}
		if _, ok = r.readUE(); !ok { // bit_depth_luma_minus8
			return info, true
		}
		if _, ok = r.readUE(); !ok { // bit_depth_chroma_minus8
			return info, true
		}
		if _, ok = r.readBit(); !ok { // qpprime_y_zero_transform_bypass_flag
			return info, true
		}
		scalingPresent, ok := r.readBit()
		if !ok {
			return info, true
		}
		if scalingPresent == 1 {
			count := 8
			if chromaFormatIDC == 3 {
				count = 12
			}
			for i := 0; i < count; i++ {
				present, ok := r.readBit()
				if !ok {
					return info, true
				}
				if present == 1 {
					size := 16
					if i >= 6 {
						size = 64
					}
					lastScale, nextScale := 8, 8
					for j := 0; j < size; j++ {
						if nextScale != 0 {
							delta, ok := r.readSE()
							if !ok {
								return info, true
							}
							nextScale = (lastScale + int(delta) + 256) % 256
						}
						if nextScale != 0 {
							lastScale = nextScale
						}
					}
				}
			}
		}
	}
	if _, ok := r.readUE(); !ok { // log2_max_frame_num_minus4
		return info, true
	}
	pocType, ok := r.readUE()
	if !ok {
		return info, true
	}
	if pocType == 0 {
		if _, ok := r.readUE(); !ok {
			return info, true
		}
	} else if pocType == 1 {
		if _, ok := r.readBit(); !ok {
			return info, true
		}
		if _, ok := r.readSE(); !ok {
			return info, true
		}
		if _, ok := r.readSE(); !ok {
			return info, true
		}
		n, ok := r.readUE()
		if !ok || n > 256 {
			return info, true
		}
		for i := uint32(0); i < n; i++ {
			if _, ok := r.readSE(); !ok {
				return info, true
			}
		}
	}
	if _, ok := r.readUE(); !ok { // max_num_ref_frames
		return info, true
	}
	if _, ok := r.readBit(); !ok { // gaps_in_frame_num_value_allowed_flag
		return info, true
	}
	picWidthInMbsMinus1, ok := r.readUE()
	if !ok {
		return info, true
	}
	picHeightInMapUnitsMinus1, ok := r.readUE()
	if !ok {
		return info, true
	}
	frameMbsOnlyFlag, ok := r.readBit()
	if !ok {
		return info, true
	}
	if frameMbsOnlyFlag == 0 {
		if _, ok := r.readBit(); !ok {
			return info, true
		}
	}
	if _, ok := r.readBit(); !ok { // direct_8x8_inference_flag
		return info, true
	}
	croppingFlag, ok := r.readBit()
	if !ok {
		return info, true
	}
	var cropLeft, cropRight, cropTop, cropBottom uint32
	if croppingFlag == 1 {
		var ok1, ok2, ok3, ok4 bool
		cropLeft, ok1 = r.readUE()
		cropRight, ok2 = r.readUE()
		cropTop, ok3 = r.readUE()
		cropBottom, ok4 = r.readUE()
		if !ok1 || !ok2 || !ok3 || !ok4 {
			return info, true
		}
	}
	width := int(picWidthInMbsMinus1+1) * 16
	height := int(2-frameMbsOnlyFlag) * int(picHeightInMapUnitsMinus1+1) * 16
	chromaArrayType := chromaFormatIDC
	if separateColourPlane != 0 {
		chromaArrayType = 0
	}
	var cropUnitX, cropUnitY int
	if chromaArrayType == 0 {
		cropUnitX = 1
		cropUnitY = int(2 - frameMbsOnlyFlag)
	} else {
		subWidthC, subHeightC := 2, 2
		if chromaFormatIDC == 3 {
			subWidthC, subHeightC = 1, 1
		} else if chromaFormatIDC == 2 {
			subHeightC = 1
		}
		cropUnitX = subWidthC
		cropUnitY = subHeightC * int(2-frameMbsOnlyFlag)
	}
	width -= int(cropLeft+cropRight) * cropUnitX
	height -= int(cropTop+cropBottom) * cropUnitY
	if width > 0 && height > 0 {
		info.Width = width
		info.Height = height
		info.HasDimensions = true
	}
	return info, true
}

func (s h264SPSInfo) dimensionsString() string {
	if !s.HasDimensions || s.Width <= 0 || s.Height <= 0 {
		return ""
	}
	return fmt.Sprintf("%dx%d", s.Width, s.Height)
}
