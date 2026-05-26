package generator

import "flag"

type Settings struct {
	Paths string // source_relative etc., passed through to protoc convention
}

func (s *Settings) RegisterFlags(fs *flag.FlagSet) {
	fs.StringVar(&s.Paths, "paths", "", "paths mode (source_relative)")
}
