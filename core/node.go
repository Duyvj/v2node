package core

import (
	"fmt"

	panel "github.com/wyx2685/v2node/api/v2board"
)

func (v *V2Core) AddNode(tag string, info *panel.NodeInfo) error {
	inBoundConfig, err := buildInbound(info, tag)
	if err != nil {
		return fmt.Errorf("build inbound error: %s", err)
	}
	err = v.addInbound(inBoundConfig)
	if err != nil {
		return fmt.Errorf("add inbound error: %s", err)
	}
	if err := v.watchCertificate(tag, info); err != nil {
		_ = v.removeInbound(tag)
		return err
	}
	return nil
}

func (v *V2Core) DelNode(tag string) error {
	v.certificates.mu.Lock()
	delete(v.certificates.files, tag)
	v.certificates.mu.Unlock()
	err := v.removeInbound(tag)
	if err != nil {
		return fmt.Errorf("remove in error: %s", err)
	}
	return nil
}
