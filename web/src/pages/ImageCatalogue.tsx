import { Link } from "react-router-dom";
import { bytes, get, post } from "../api";
import { useLive } from "../live";
import { ErrorBox, Modal, useAction } from "../ui";
import "./imagecat.css";

export interface CatalogueImage {
  id: string;
  name: string;
  version: string;
  url: string;
  algorithm: string;
  defaultUser?: string;
  volumeName: string;
  cloudInit: boolean;
  sizeBytes?: number;
  present: boolean;
  downloading: boolean;
}
interface PoolImages {
  images: CatalogueImage[];
  downloadable: boolean;
  reason?: string;
  running: string[];
}

/**
 * Well-known cloud images the node downloads itself and verifies by checksum.
 * The checksum comes from the same project as the image: it catches a corrupt
 * download, not a compromised mirror.
 */
export default function ImageCatalogue(props: { node: number; pool: string; onClose: () => void }) {
  const base = `/nodes/${props.node}/pools/${encodeURIComponent(props.pool)}/images`;
  const list = useLive(() => get<PoolImages>(base), base);
  const act = useAction();
  // The dialog closes once the server accepted a download; the Tasks dock follows it and stops it.
  const start = async (img: CatalogueImage) => {
    if (await act.run(() => post(base, { id: img.id }))) props.onClose();
  };
  const running = new Set(list.data?.running ?? []);

  return (
    <Modal title="Image catalogue" onClose={props.onClose} wide>
      <p className="muted catnote">
        Downloads into <b>{props.pool}</b>. The checksum comes from the same project as the image: it detects a damaged download, not a compromised mirror.
      </p>
      <ErrorBox error={list.error ?? act.error} />
      {list.data && !list.data.downloadable && <div className="empty">{list.data.reason ?? "This pool cannot be downloaded into."}</div>}
      {list.data?.downloadable && (
        <ul className="catlist">
          {list.data.images.map((img) => {
            const busy = running.has(img.volumeName);
            return (
              <li key={img.id} className="catitem">
                <div className="catmain">
                  <div className="cattitle">
                    <b>{img.name}</b>
                    <span className="badge">{img.cloudInit ? "cloud-init" : "no cloud-init"}</span>
                    {img.present && <span className="badge catok">Already in this pool</span>}
                  </div>
                  <small className="muted">
                    ~{bytes(img.sizeBytes ?? 0)}
                    {img.defaultUser && (
                      <>
                        {" · login "}
                        <span className="mono">{img.defaultUser}</span>
                      </>
                    )}
                    {" · "}
                    <span className="mono catvol">{img.volumeName}</span>
                  </small>
                </div>
                <div className="catact">
                  {busy ? (
                    <span className="muted">Downloading</span>
                  ) : img.present ? (
                    <Link className="button" to={`/datacenter/vms?new=${props.node}&pool=${encodeURIComponent(props.pool)}&image=${encodeURIComponent(img.volumeName)}`} onClick={props.onClose}>
                      Create VM from this image
                    </Link>
                  ) : (
                    <button className="primary" disabled={act.busy} onClick={() => start(img)}>
                      Download
                    </button>
                  )}
                </div>
              </li>
            );
          })}
        </ul>
      )}
      <footer>
        <button onClick={props.onClose}>Close</button>
      </footer>
    </Modal>
  );
}
