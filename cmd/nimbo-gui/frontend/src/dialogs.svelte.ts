// In-app replacements for alert(), confirm() and prompt(). The browser's own
// boxes are titled "wails.localhost says", which means nothing to anyone and
// ignores the app's theme; these carry a real title and match the windows they
// open in. Dialog.svelte, mounted once in App.svelte, renders whichever is open.

type Kind = "ask" | "tell" | "text";

export type DialogReq = {
  kind: Kind;
  title: string;
  message: string;
  ok: string;
  cancel: string;
  danger: boolean;
  value: string;
  resolve: (v: any) => void;
};

export const dialog = $state<{ current: DialogReq | null }>({ current: null });

// A second request while one is open waits its turn rather than replacing it.
const queue: DialogReq[] = [];

function open<T>(r: Omit<DialogReq, "resolve">): Promise<T> {
  return new Promise<T>((resolve) => {
    const req = { ...r, resolve };
    if (dialog.current) queue.push(req);
    else dialog.current = req;
  });
}

// closeDialog answers the open dialog and shows the next one, if any.
export function closeDialog(v: unknown) {
  const r = dialog.current;
  dialog.current = queue.shift() ?? null;
  r?.resolve(v);
}

// ask is confirm(): true when the user picks ok.
export function ask(o: { title: string; message: string; ok?: string; cancel?: string; danger?: boolean }): Promise<boolean> {
  return open<boolean>({
    kind: "ask", title: o.title, message: o.message,
    ok: o.ok ?? "OK", cancel: o.cancel ?? "Cancel", danger: !!o.danger, value: "",
  });
}

// tell is alert(): resolves once it is dismissed.
export function tell(o: { title: string; message: string; ok?: string }): Promise<void> {
  return open<void>({
    kind: "tell", title: o.title, message: o.message,
    ok: o.ok ?? "OK", cancel: "", danger: false, value: "",
  });
}

// askText is prompt(): the text entered, or null when cancelled.
export function askText(o: { title: string; message: string; value?: string; ok?: string }): Promise<string | null> {
  return open<string | null>({
    kind: "text", title: o.title, message: o.message,
    ok: o.ok ?? "OK", cancel: "Cancel", danger: false, value: o.value ?? "",
  });
}
