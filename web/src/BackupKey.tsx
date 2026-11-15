import { get } from "./api";
import { useLive } from "./live";

/** What the server says about backup encryption (GET /api/backup-encryption). */
export interface BackupEncryption {
  on: boolean;
  hasKey: boolean;
  /** The public age key backups are encrypted to. */
  recipient?: string;
  since?: number;
  /** With ?count=1: backups that need the key. */
  vmBackups?: number;
  controllerBackups?: number;
}

/** The state of backup encryption, refreshed when it changes (event "backupkey"). */
export function useBackupEncryption(count = false) {
  const path = "/backup-encryption" + (count ? "?count=1" : "");
  return useLive(() => get<BackupEncryption>(path), path, (e) => e.type === "backupkey");
}

/** A green dot with "Encrypted", or a grey one with "Not encrypted". */
export function EncDot({ on, title }: { on: boolean; title?: string }) {
  return (
    <span className={"badge " + (on ? "s-encrypted" : "s-unencrypted")} title={title}>
      {on ? "Encrypted" : "Not encrypted"}
    </span>
  );
}

/** The dot for the header of a backups page: whether new backups are encrypted. */
export function NewBackupsDot() {
  const st = useBackupEncryption();
  if (!st.data) return null;
  return <EncDot on={st.data.on} title={st.data.on ? "New backups are encrypted to the backup key (Settings > Backup encryption)" : "New backups are not encrypted (Settings > Backup encryption)"} />;
}
