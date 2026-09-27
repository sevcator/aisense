package hitchance

func (c Config) Clone() Config {
	out := c
	if c.Rules == nil {
		return out
	}
	out.Rules = append([]Rule{}, c.Rules...)
	for i, r := range c.Rules {
		out.Rules[i].Messages = append([]string(nil), r.Messages...)
		out.Rules[i].StatusCodes = append([]int(nil), r.StatusCodes...)
		out.Rules[i].Models = append([]string(nil), r.Models...)
		out.Rules[i].UpstreamIDs = append([]string(nil), r.UpstreamIDs...)
	}
	return out
}
