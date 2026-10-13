import { createFileRoute } from "@tanstack/react-router";
import { BlobPage } from "@/features/files/BlobPage";

export const Route = createFileRoute("/$ns/$name/blob/$rev/$")({
  component: Page,
});

function Page() {
  const { ns, name, rev, _splat } = Route.useParams();
  return <BlobPage kind="model" ns={ns} name={name} rev={rev} path={_splat ?? ""} />;
}
