package config

import "github.com/livekit/protocol/livekit"

type PCWrapper livekit.ParticipantCompositeEgressRequest

func (p *PCWrapper) GetFileOutputs() []*livekit.EncodedFileOutput {
	return (*livekit.ParticipantCompositeEgressRequest)(p).GetFileOutputs()
}

func (p *PCWrapper) GetStreamOutputs() []*livekit.StreamOutput {
	return (*livekit.ParticipantCompositeEgressRequest)(p).GetStreamOutputs()
}

func (p *PCWrapper) GetSegmentOutputs() []*livekit.SegmentedFileOutput {
	return (*livekit.ParticipantCompositeEgressRequest)(p).GetSegmentOutputs()
}

func (p *PCWrapper) GetFile() *livekit.EncodedFileOutput {
	return nil
}

func (p *PCWrapper) GetStream() *livekit.StreamOutput {
	return nil
}

func (p *PCWrapper) GetSegments() *livekit.SegmentedFileOutput {
	return nil
}
