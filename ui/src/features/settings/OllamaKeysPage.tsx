import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { Trash2 } from "lucide-react";
import { useState } from "react";
import { api } from "@/api/client";
import { PageShell } from "@/components/PageShell";
import { Button } from "@/components/ui/button";
import { ConfirmDialog } from "@/components/ui/confirm";
import { Field, Input, Textarea } from "@/components/ui/form";
import { EmptyState, ErrorState, InlineError, LoadingRows } from "@/components/ui/states";
import { relativeTime } from "@/lib/format";
import { fieldErrors, unwrap } from "@/lib/problem";

/** Ollama public keys, so `ollama pull` works for private Repos (AUTH-7). */
export function OllamaKeysPage() {
  const qc = useQueryClient();
  const keys = useQuery({
    queryKey: ["me", "ollama-keys"],
    queryFn: async () => unwrap(await api.GET("/me/ollama-keys")),
  });
  const [name, setName] = useState("");
  const [key, setKey] = useState("");
  const add = useMutation({
    mutationFn: async () =>
      unwrap(
        await api.POST("/me/ollama-keys", {
          body: { name: name || undefined, public_key: key.trim() },
        }),
      ),
    onSuccess: () => {
      setName("");
      setKey("");
      qc.invalidateQueries({ queryKey: ["me", "ollama-keys"] });
    },
  });
  const errs = fieldErrors(add.error);
  return (
    <PageShell
      title="Ollama keys"
      subtitle="Register your Ollama public key to pull private GGUF Repos with ollama pull."
    >
      {keys.isPending ? (
        <LoadingRows rows={2} label="Loading keys" />
      ) : keys.isError ? (
        <ErrorState error={keys.error} onRetry={() => keys.refetch()} />
      ) : keys.data.items.length === 0 ? (
        <EmptyState title="No keys yet">
          Public Repos work without a key. Add one for private and internal Repos.
        </EmptyState>
      ) : (
        <ul className="hg-enter divide-y divide-line rounded-lg border border-line bg-surface">
          {keys.data.items.map((k) => (
            <li
              key={k.id}
              className="flex flex-wrap items-center gap-x-3 gap-y-1 px-4 py-2.5 text-sm"
            >
              <span className="font-medium">{k.name || "Unnamed key"}</span>
              <span className="break-all font-mono text-xs text-muted">{k.fingerprint}</span>
              <span className="tabular text-xs text-muted">Added {relativeTime(k.created_at)}</span>
              <ConfirmDialog
                trigger={
                  <Button
                    size="icon-sm"
                    variant="ghost"
                    className="ml-auto"
                    aria-label={`Remove ${k.name || k.fingerprint}`}
                  >
                    <Trash2 aria-hidden="true" />
                  </Button>
                }
                title="Remove this key?"
                actionLabel="Remove key"
                onConfirm={async () => {
                  const res = await api.DELETE("/me/ollama-keys/{id}", {
                    params: { path: { id: k.id } },
                  });
                  if (!res.response.ok) unwrap(res);
                  qc.invalidateQueries({ queryKey: ["me", "ollama-keys"] });
                }}
              >
                <p>Ollama on that machine can no longer pull your private Repos.</p>
              </ConfirmDialog>
            </li>
          ))}
        </ul>
      )}
      <form
        className="max-w-xl space-y-3"
        onSubmit={(e) => {
          e.preventDefault();
          add.mutate();
        }}
      >
        <h2 className="pt-2 font-semibold">Add a key</h2>
        <Field label="Name" hint="Optional, such as the machine's name.">
          {(p) => <Input {...p} value={name} onChange={(e) => setName(e.target.value)} />}
        </Field>
        <Field
          label="Public key"
          hint="Paste the contents of ~/.ollama/id_ed25519.pub."
          error={errs.public_key}
        >
          {(p) => (
            <Textarea
              {...p}
              required
              rows={3}
              className="font-mono text-xs"
              placeholder="ssh-ed25519 AAAA…"
              value={key}
              onChange={(e) => setKey(e.target.value)}
            />
          )}
        </Field>
        {add.error && !errs.public_key ? <InlineError error={add.error} /> : null}
        <Button type="submit" variant="primary" disabled={!key.trim() || add.isPending}>
          Add key
        </Button>
      </form>
    </PageShell>
  );
}
