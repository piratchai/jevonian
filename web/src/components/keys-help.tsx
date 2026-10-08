function isHttpUrl(value?: string): boolean {
  return Boolean(value && /^https?:\/\//i.test(value.trim()));
}

/** Short help + optional link for creating or finding an API key / signing in. */
export function KeysHelp({
  keysUrl,
  hint,
  linkLabel = "Get an API key",
}: {
  keysUrl?: string;
  hint?: string;
  linkLabel?: string;
}) {
  const url = keysUrl?.trim() || (isHttpUrl(hint) ? hint!.trim() : undefined);
  const text = hint && !isHttpUrl(hint) ? hint : undefined;
  if (!url && !text) return null;

  return (
    <p className="text-xs text-kumo-subtle">
      {url ? (
        <>
          <a
            href={url}
            target="_blank"
            rel="noreferrer"
            className="font-medium text-kumo-default underline underline-offset-2 hover:text-kumo-brand"
          >
            {linkLabel}
          </a>
          {text ? <> — {text}</> : null}
        </>
      ) : (
        text
      )}
    </p>
  );
}
