import {
  createContext,
  type ReactNode,
  useCallback,
  useContext,
  useMemo,
  useRef,
  useState,
} from "react";
import { cn } from "@/lib/utils";

type Toast = { id: number; message: string; tone: "ok" | "danger" };

const ToastContext = createContext<(message: string, tone?: "ok" | "danger") => void>(() => {});

/** Shows short confirmations in a live region at the bottom of the screen. */
export function ToastProvider({ children }: { children: ReactNode }) {
  const [toasts, setToasts] = useState<Toast[]>([]);
  const next = useRef(1);
  const show = useCallback((message: string, tone: "ok" | "danger" = "ok") => {
    const id = next.current++;
    setToasts((t) => [...t, { id, message, tone }]);
    setTimeout(() => setToasts((t) => t.filter((x) => x.id !== id)), 5000);
  }, []);
  const value = useMemo(() => show, [show]);
  return (
    <ToastContext.Provider value={value}>
      {children}
      <div
        aria-live="polite"
        className="pointer-events-none fixed inset-x-0 bottom-4 z-[60] flex flex-col items-center gap-2 px-4"
      >
        {toasts.map((t) => (
          <div
            key={t.id}
            role="status"
            className={cn(
              "pointer-events-auto rounded-md border px-4 py-2 text-sm shadow-lg",
              t.tone === "ok"
                ? "border-line bg-steel-900 text-steel-50"
                : "border-danger/40 bg-danger-soft text-danger",
            )}
          >
            {t.message}
          </div>
        ))}
      </div>
    </ToastContext.Provider>
  );
}

/** Returns a function that shows a toast. */
export function useToast() {
  return useContext(ToastContext);
}
