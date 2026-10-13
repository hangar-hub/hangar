import { useQuery } from "@tanstack/react-query";
import { Link } from "@tanstack/react-router";
import { api } from "@/api/client";
import { Logo } from "@/components/Logo";
import { ScrollRegion } from "@/components/ScrollRegion";
import { Button } from "@/components/ui/button";
import { Skeleton } from "@/components/ui/states";
import { unwrap } from "@/lib/problem";
import { useMe } from "@/lib/session";
import { ArsenalPage } from "./arsenal/ArsenalPage";

const clients = [
  "huggingface_hub",
  "transformers",
  "datasets",
  "git",
  "Ollama",
  "LM Studio",
  "vLLM",
  "DuckDB",
];

function Landing() {
  const providers = useQuery({
    queryKey: ["auth", "providers"],
    queryFn: async () => unwrap(await api.GET("/auth/providers")),
  });
  const host = window.location.host;
  const origin = window.location.origin;
  return (
    <section
      aria-labelledby="page-title"
      className="grid items-center gap-10 py-6 lg:grid-cols-[minmax(0,1fr)_minmax(0,1fr)] lg:py-16"
    >
      <div className="max-w-xl">
        <Logo className="size-12" />
        <h1
          id="page-title"
          className="mt-6 text-4xl font-semibold leading-[1.05] tracking-tight text-balance sm:text-5xl"
        >
          A self-hosted home for your arsenal of models.
        </h1>
        <p className="mt-5 max-w-prose text-lg text-muted">
          Hangar keeps models and datasets with their full history, pulls them from huggingface.co
          once, and serves them to the tools you already use.
        </p>
        <div className="mt-8 flex flex-wrap gap-3">
          <Button asChild variant="primary" size="lg">
            <Link to="/auth/login">Sign in</Link>
          </Button>
          {providers.data?.registration === "open" ? (
            <Button asChild size="lg">
              <Link to="/auth/login" search={{ register: "1" }}>
                Create an account
              </Link>
            </Button>
          ) : null}
        </div>
      </div>
      <div>
        <div className="overflow-hidden rounded-lg border border-steel-700 bg-steel-900 text-steel-100 shadow-xl">
          <div
            className="flex items-center gap-1.5 border-b border-steel-700 px-4 py-2.5"
            aria-hidden="true"
          >
            <span className="size-2.5 rounded-full bg-steel-600" />
            <span className="size-2.5 rounded-full bg-steel-600" />
            <span className="size-2.5 rounded-full bg-amber-400" />
          </div>
          <ScrollRegion label="Example commands" className="overflow-y-hidden">
            <pre className="px-4 py-4 font-mono text-[13px] leading-6">
              <code>
                <span className="text-steel-400"># Point any Hugging Face client at Hangar</span>
                {"\n"}
                <span className="text-amber-300">$</span> export HF_ENDPOINT={origin}
                {"\n"}
                <span className="text-amber-300">$</span> hf download
                TinyLlama/TinyLlama-1.1B-Chat-GGUF
                {"\n\n"}
                <span className="text-steel-400"># Or pull one quant straight into Ollama</span>
                {"\n"}
                <span className="text-amber-300">$</span> ollama pull {host}
                /TinyLlama/TinyLlama-1.1B-Chat-GGUF:Q4_K_M
              </code>
            </pre>
          </ScrollRegion>
        </div>
        <ul
          aria-label="Works with"
          className="mt-4 flex flex-wrap gap-x-4 gap-y-1 font-mono text-xs text-muted"
        >
          {clients.map((c) => (
            <li key={c}>{c}</li>
          ))}
        </ul>
      </div>
    </section>
  );
}

/** `/`: the landing page when signed out, the Arsenal when signed in. */
export function HomePage() {
  const me = useMe();
  if (me.isPending) return <Skeleton className="h-64 w-full" />;
  return me.data ? <ArsenalPage /> : <Landing />;
}
