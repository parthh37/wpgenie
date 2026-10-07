import { ask, askText } from "@/components/app/confirm"
import { notify } from "@/components/app/toaster"
import { api, ApiError } from "@/lib/api"
import type { Site } from "@/lib/types"
import { download, filesPath, joinPath, resolvePath, type CopyResult, type Entry } from "./lib"

// What a row's menu does. Each resolves once done (or cancelled); failures
// throw, for the caller's showError.

export type FileAct = "edit" | "download" | "rename" | "copy" | "mode" | "extract" | "delete"

export async function fileAction(site: Site, dir: string, x: Entry, act: FileAct, { open, reload }: { open: () => unknown; reload: () => unknown }) {
  const path = joinPath(dir, x.name)
  const isDir = x.type === "dir"
  const q = (sub: string, params: Record<string, string | number | null>) => filesPath(site, sub, params)
  switch (act) {
    case "edit":
      return open()
    case "download":
      return download(site, path)
    case "rename": {
      const to = await askText(`Type a new name, or a path to move it: other/folder/${x.name} (from here) or /wp-content/… (from the top).`, {
        title: `Rename ${x.name}`,
        label: "New name",
        value: x.name,
        ok: "Rename",
        danger: false,
      })
      if (!to || to === x.name) return
      await api("POST", q("/move", { path, to: resolvePath(dir, to) }))
      notify(`Moved to ${resolvePath(dir, to)}`)
      return reload()
    }
    case "copy": {
      const dot = isDir ? -1 : x.name.lastIndexOf(".")
      const suggestion = dot > 0 ? `${x.name.slice(0, dot)}-copy${x.name.slice(dot)}` : `${x.name}-copy`
      const to = await askText("Links inside aren't copied; nothing existing is replaced.", {
        title: `Copy ${x.name}`,
        label: "Name of the copy",
        value: suggestion,
        ok: "Copy",
      })
      if (!to) return
      const r = await api<CopyResult>("POST", q("/copy", { path, to: resolvePath(dir, to) }))
      notify(isDir ? `Copied ${r.files} file(s) in ${r.folders} folder(s)` : `Copied to ${resolvePath(dir, to)}`)
      return reload()
    }
    case "mode": {
      const mode = await askText(
        isDir
          ? "Folders are usually 755: the site keeps full access (7), others may enter and list (5)."
          : "Files are usually 644 (the site writes, everyone reads); 600 or 640 keeps a file from other users, 444 locks it.",
        { title: `Permissions of ${x.name}`, label: "Octal permissions", value: x.mode.replace(/^0/, ""), ok: "Change", danger: false }
      )
      if (!mode) return
      await api("PUT", q("/mode", { path, mode }))
      return reload()
    }
    case "extract": {
      const params = { path, to: dir }
      try {
        const r = await api<CopyResult>("POST", q("/extract", params))
        notify(`Extracted ${r.files} file(s)` + (r.skipped ? `; ${r.skipped} link(s) or special entries skipped` : ""))
      } catch (e) {
        if (!(e instanceof ApiError) || e.status !== 409 || !/already exist/.test(e.message)) throw e
        const which = e.message.replace(/^conflict: /, "").replace(/ already exist.*$/, "")
        if (!(await ask(`Replace existing files? ${which} already exist; the archive's versions would replace them.`, { ok: "Replace", danger: true }))) return
        const r = await api<CopyResult>("POST", q("/extract", { ...params, overwrite: 1 }))
        notify(`Extracted ${r.files} file(s), replacing existing ones`)
      }
      return reload()
    }
    case "delete": {
      const what = isDir ? `${path} and everything in it` : path
      if (!(await ask(`Delete ${x.name}? This permanently deletes ${what}. Backups, if any, still have it.`))) return
      await api("DELETE", q("", { path }))
      notify(`Deleted ${path}`)
      return reload()
    }
  }
}
