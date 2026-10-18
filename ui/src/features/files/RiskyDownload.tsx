import { Download } from "lucide-react";
import { Button } from "@/components/ui/button";
import { ConfirmDialog } from "@/components/ui/confirm";
import { baseName } from "@/lib/repo";
import type { ScanStatus } from "../repos/badges";

/**
 * Downloads a pickle file the scan marked unsafe or suspicious, or has not
 * scanned yet, after the user confirms, with accept_risk=1, which
 * Instances that block unsafe downloads require; suspicious files need it
 * when scan.block_level is suspicious (07-indexing-and-scanning.md).
 */
export function RiskyDownload({
  href,
  path,
  status,
  navigate = (url) => window.location.assign(url),
}: {
  href: string;
  path: string;
  status: ScanStatus;
  navigate?: (url: string) => void;
}) {
  return (
    <ConfirmDialog
      trigger={
        <Button size="sm" variant="ghost">
          <Download aria-hidden="true" />
          Download
        </Button>
      }
      title={`Download ${baseName(path)}?`}
      actionLabel="Download anyway"
      onConfirm={async () => {
        navigate(`${href}${href.includes("?") ? "&" : "?"}accept_risk=1`);
      }}
    >
      <p>
        {status === "unsafe"
          ? "The pickle scan found imports in this file that can run code or touch the system when it is loaded."
          : status === "suspicious"
            ? "The pickle scan found imports in this file that it does not know to be safe. Loading them can run code."
            : "The pickle scan has not checked this file yet. Loading a pickle file can run code."}{" "}
        Only load it with pickle or torch.load if you trust where it came from.
      </p>
    </ConfirmDialog>
  );
}
