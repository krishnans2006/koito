import { useEffect, useState } from "react";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { useRevalidator } from "react-router";
import {
  getHiddenArtists,
  searchVisibilityArtists,
  setArtistHidden,
  type VisibilityArtist,
} from "api/api";
import { useAppContext } from "~/providers/AppProvider";
import SubHeader from "../primitives/SubHeader";
import { AsyncButton } from "../AsyncButton";

const adminQueryKey = "admin-artist-visibility";

export default function HiddenArtists() {
  const [query, setQuery] = useState("");
  const [debouncedQuery, setDebouncedQuery] = useState("");
  const [saving, setSaving] = useState<number>();
  const [saveError, setSaveError] = useState("");
  const [success, setSuccess] = useState("");
  const queryClient = useQueryClient();
  const revalidator = useRevalidator();
  const { refreshWebsite } = useAppContext();

  useEffect(() => {
    const timeout = setTimeout(() => setDebouncedQuery(query.trim()), 250);
    return () => clearTimeout(timeout);
  }, [query]);

  const hidden = useQuery({
    queryKey: [adminQueryKey, "hidden"],
    queryFn: ({ signal }) => getHiddenArtists(signal),
  });
  const results = useQuery({
    queryKey: [adminQueryKey, "search", debouncedQuery],
    queryFn: ({ signal }) => searchVisibilityArtists(debouncedQuery, signal),
    enabled: debouncedQuery.length > 0,
  });

  async function update(artist: VisibilityArtist, hide: boolean) {
    setSaving(artist.id);
    setSaveError("");
    setSuccess("");
    try {
      await setArtistHidden(artist.id, hide);
    } catch (error) {
      setSaveError(error instanceof Error ? error.message : "Could not save changes");
      setSaving(undefined);
      return;
    }

    setSuccess(`${artist.name} is now ${hide ? "hidden" : "visible"}.`);
    // Cancel old responses before resetting active views. This also removes
    // inactive cached queries so navigating back cannot restore hidden data.
    const websiteQueries = { predicate: (q: { queryKey: readonly unknown[] }) =>
      q.queryKey[0] !== adminQueryKey && q.queryKey[0] !== "api-keys" };
    await queryClient.cancelQueries(websiteQueries);
    await queryClient.resetQueries(websiteQueries);
    queryClient.removeQueries({ ...websiteQueries, type: "inactive" });
    refreshWebsite();
    void revalidator.revalidate();
    await queryClient.invalidateQueries({ queryKey: [adminQueryKey] });
    setSaving(undefined);
  }

  const artistRow = (artist: VisibilityArtist, isHidden: boolean) => (
    <li key={artist.id} className="flex items-center justify-between gap-3 border rounded p-3">
      <div className="min-w-0">
        <p className="font-medium break-words">{artist.name}</p>
        <p className="text-sm color-fg-secondary break-all">
          {artist.musicbrainz_id ? `MusicBrainz: ${artist.musicbrainz_id}` : `Artist ID: ${artist.id}`}
        </p>
      </div>
      <AsyncButton
        loading={saving === artist.id}
        disabled={saving !== undefined}
        onClick={() => void update(artist, !isHidden)}
      >
        {isHidden ? "Unhide" : "Add"}
      </AsyncButton>
    </li>
  );

  return (
    <div className="flex flex-col gap-4">
      <SubHeader>Hidden artists</SubHeader>
      <p>Hidden artists and their tracks are excluded from website views and statistics. Their scrobbles remain stored and included in exports.</p>
      <p className="text-sm color-fg-secondary">Collaborations with a hidden artist are also hidden. Other tracks on mixed compilations remain visible.</p>
      {saveError && <p role="alert" className="error">{saveError}</p>}
      {success && <p role="status" className="success">{success}</p>}
      <label htmlFor="hidden-artist-search" className="font-medium">Add an artist</label>
      <input
        id="hidden-artist-search"
        type="search"
        value={query}
        onChange={(event) => setQuery(event.target.value)}
        maxLength={500}
        placeholder="Search artist names or aliases"
        className="w-full fg bg rounded p-2 border"
      />
      {query.trim() && (
        query.trim() !== debouncedQuery || results.isPending ? <p role="status">Searching…</p> :
        results.isError ? <p role="alert" className="error">{results.error.message}</p> :
        <>
          <ul className="flex flex-col gap-2" aria-label="Artist search results">
            {results.data?.map((artist) => artistRow(artist, artist.hidden))}
          </ul>
          {results.data?.length === 0 && <p>No matching artists.</p>}
          {results.data?.length === 30 && <p className="text-sm">Showing the first 30 matches. Refine your search to find more.</p>}
        </>
      )}
      <h3 className="font-medium mt-3">Currently hidden</h3>
      {hidden.isPending ? <p role="status">Loading hidden artists…</p> :
       hidden.isError ? <p role="alert" className="error">{hidden.error.message}</p> :
       hidden.data.length === 0 ? <p>No artists are hidden.</p> :
       <ul className="flex flex-col gap-2" aria-label="Hidden artists">
         {hidden.data.map((artist) => artistRow(artist, true))}
       </ul>}
    </div>
  );
}
