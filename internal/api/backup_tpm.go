package api

// The state of a VM's emulated TPM 2.0 in backups. swtpm keeps it in one small file, which a backup stores whole
// (backupTPMFile, in every backup of a chain, with its SHA-256 in the manifest) and a restore puts in place for
// the restored VM with the node command's tpm-place, the verb a failover of a replica uses: the state directory
// is made as libvirt makes it and handed to the swtpm account; libvirt labels it for SELinux or AppArmor when the
// VM starts. The directory is named after the VM's UUID, so the state is placed once the restored VM is defined
// and has its UUID (a new one for a restore as a new VM).

import (
	"context"
	"fmt"
)

// tpmRestoreWarning is what a restore of the backup says about the TPM: nothing when the backup holds its state.
func tpmRestoreWarning(man *backupManifest) string {
	if man.TPMState != "" || !man.TPM {
		return ""
	}
	return "the VM had a TPM; its state is not part of this backup, the restored VM starts with an empty TPM"
}

// stageTPM copies the TPM state of the backup into the work directory on the target node and checks it there
// against the manifest. It returns the staged file, or "" for a backup without TPM state.
func (p *restorePlan) stageTPM(ctx context.Context, wd *bkWork) (string, error) {
	if p.man.TPMState == "" {
		return "", nil
	}
	from, stage := p.src.dir+"/"+p.man.TPMState, wd.file(backupTPMFile)
	var err error
	if p.remote {
		err = copyToNode(ctx, p.src.h, p.dst, p.ep, from, stage, "600", nil)
	} else {
		err = p.dst.Copy(from, stage)
	}
	if err != nil {
		return "", fmt.Errorf("copying the TPM state: %s", impFirstLine(err.Error()))
	}
	sum, err := p.dst.SHA256(stage)
	if err != nil {
		return "", err
	}
	if sum != p.man.TPMSHA256 {
		return "", fmt.Errorf("%s of the backup %s is damaged: its SHA-256 is %s, the manifest says %s", p.man.TPMState, p.src.name, sum, p.man.TPMSHA256)
	}
	return stage, nil
}
