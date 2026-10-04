/**
 * Named goldens for the collection editor tests: the writes today's pages
 * send, as `v2Recorder.writes()` records them (wire JSON, undefined members
 * absent). A later PR changes one of these only on purpose and says why.
 *
 * None of them reads a personal collection's `groups` or `group_id`.
 */
import type { RecordedCall } from "@/test/v2Recorder";

type Writes = RecordedCall[];
type ByLimit = Record<"no limit" | "the server's no-limit sentinel" | "a limit of 250", Writes>;

export const goldens = {
  /** Admin manual create from the editor page: the POST, then the poster file, then the backdrop URL. */
  adminManualCreate: [
    {
      operation: "POST /api/v2/admin/collections",
      path: "/api/v2/admin/collections",
      headers: {},
      body: {
        title: "Staff picks",
        description: "",
        collection_type: "manual",
        visibility: "visible",
        featured: false,
        library_ids: ["1"],
      },
    },
    {
      operation: "PUT /api/v2/admin/collections/{id}/poster",
      path: "/api/v2/admin/collections/c1/poster",
      headers: {},
      form: {
        image: {
          file: "poster.png",
        },
      },
    },
    {
      operation: "PUT /api/v2/admin/collections/{id}/backdrop",
      path: "/api/v2/admin/collections/c1/backdrop",
      headers: {},
      form: {
        source_url: "https://images.example/backdrop.png",
      },
    },
  ] satisfies Writes,
  /** Admin smart create: `query_definition` keeps numeric library ids; the top-level `library_ids` are strings. */
  adminSmartCreate: [
    {
      operation: "POST /api/v2/admin/collections",
      path: "/api/v2/admin/collections",
      headers: {},
      body: {
        title: "New this month",
        description: "",
        collection_type: "smart",
        visibility: "visible",
        featured: false,
        query_definition: {
          library_ids: [1],
          match: "all",
          groups: [],
          sort: {
            field: "added_at",
            order: "desc",
          },
        },
        sort_config: {},
        library_ids: ["1"],
      },
    },
  ] satisfies Writes,
  /** Admin manual update. Today the editor PATCH carries `featured`. */
  adminManualUpdate: [
    {
      operation: "PATCH /api/v2/admin/collections/{id}",
      path: "/api/v2/admin/collections/c1",
      headers: {
        "If-Match": '"/api/v2/admin/collections/c1#1"',
      },
      body: {
        title: "Renamed",
        description: "",
        collection_type: "manual",
        visibility: "visible",
        featured: false,
        library_ids: ["1"],
      },
    },
  ] satisfies Writes,
  /** Admin staged poster removal: nothing is sent until Save, then the DELETE follows the PATCH. */
  adminStagedPosterRemoval: [
    {
      operation: "PATCH /api/v2/admin/collections/{id}",
      path: "/api/v2/admin/collections/c1",
      headers: {
        "If-Match": '"/api/v2/admin/collections/c1#1"',
      },
      body: {
        title: "Original",
        description: "",
        collection_type: "manual",
        visibility: "visible",
        featured: false,
        library_ids: ["1"],
      },
    },
    {
      operation: "DELETE /api/v2/admin/collections/{id}/image",
      path: "/api/v2/admin/collections/c1/image",
      headers: {},
      query: {
        type: "poster",
      },
    },
  ] satisfies Writes,
  /** Admin backdrop removed and then replaced by a file: the upload replaces it and no DELETE is sent. */
  adminBackdropReplacement: [
    {
      operation: "PATCH /api/v2/admin/collections/{id}",
      path: "/api/v2/admin/collections/c1",
      headers: {
        "If-Match": '"/api/v2/admin/collections/c1#1"',
      },
      body: {
        title: "Original",
        description: "",
        collection_type: "manual",
        visibility: "visible",
        featured: false,
        library_ids: ["1"],
      },
    },
    {
      operation: "PUT /api/v2/admin/collections/{id}/backdrop",
      path: "/api/v2/admin/collections/c1/backdrop",
      headers: {},
      form: {
        image: {
          file: "backdrop.png",
        },
      },
    },
  ] satisfies Writes,
  /** A loaded admin smart collection saved unchanged. The sentinel saves as no limit; the PATCH carries `featured`. */
  adminSmartUnchanged: {
    "no limit": [
      {
        operation: "PATCH /api/v2/admin/collections/{id}",
        path: "/api/v2/admin/collections/c1",
        headers: {
          "If-Match": '"/api/v2/admin/collections/c1#1"',
        },
        body: {
          title: "Original",
          description: "",
          collection_type: "smart",
          visibility: "visible",
          featured: false,
          query_definition: {
            library_ids: [1],
            match: "all",
            groups: [
              {
                match: "all",
                rules: [
                  {
                    field: "genre",
                    op: "is",
                    value: "Comedy",
                  },
                ],
              },
            ],
            sort: {
              field: "added_at",
              order: "desc",
            },
          },
          sort_config: {},
          library_ids: ["1"],
        },
      },
    ],
    "the server's no-limit sentinel": [
      {
        operation: "PATCH /api/v2/admin/collections/{id}",
        path: "/api/v2/admin/collections/c1",
        headers: {
          "If-Match": '"/api/v2/admin/collections/c1#1"',
        },
        body: {
          title: "Original",
          description: "",
          collection_type: "smart",
          visibility: "visible",
          featured: false,
          query_definition: {
            library_ids: [1],
            match: "all",
            groups: [
              {
                match: "all",
                rules: [
                  {
                    field: "genre",
                    op: "is",
                    value: "Comedy",
                  },
                ],
              },
            ],
            sort: {
              field: "added_at",
              order: "desc",
            },
          },
          sort_config: {},
          library_ids: ["1"],
        },
      },
    ],
    "a limit of 250": [
      {
        operation: "PATCH /api/v2/admin/collections/{id}",
        path: "/api/v2/admin/collections/c1",
        headers: {
          "If-Match": '"/api/v2/admin/collections/c1#1"',
        },
        body: {
          title: "Original",
          description: "",
          collection_type: "smart",
          visibility: "visible",
          featured: false,
          query_definition: {
            library_ids: [1],
            match: "all",
            groups: [
              {
                match: "all",
                rules: [
                  {
                    field: "genre",
                    op: "is",
                    value: "Comedy",
                  },
                ],
              },
            ],
            sort: {
              field: "added_at",
              order: "desc",
            },
            limit: 250,
          },
          sort_config: {},
          library_ids: ["1"],
        },
      },
    ],
  } satisfies ByLimit,
  /** A loaded personal smart collection saved unchanged; the PATCH carries no `description`. */
  personalSmartUnchanged: {
    "no limit": [
      {
        operation: "PATCH /api/v2/collections/{id}",
        path: "/api/v2/collections/c1",
        headers: {
          "If-Match": '"/api/v2/collections/c1#1"',
        },
        body: {
          name: "Rainy days",
          is_shared: false,
          query_definition: {
            library_ids: [1],
            match: "all",
            groups: [
              {
                match: "all",
                rules: [
                  {
                    field: "genre",
                    op: "is",
                    value: "Comedy",
                  },
                ],
              },
            ],
            sort: {
              field: "added_at",
              order: "desc",
            },
          },
          sort_config: {},
          include_in_server_collections: false,
        },
      },
    ],
    "the server's no-limit sentinel": [
      {
        operation: "PATCH /api/v2/collections/{id}",
        path: "/api/v2/collections/c1",
        headers: {
          "If-Match": '"/api/v2/collections/c1#1"',
        },
        body: {
          name: "Rainy days",
          is_shared: false,
          query_definition: {
            library_ids: [1],
            match: "all",
            groups: [
              {
                match: "all",
                rules: [
                  {
                    field: "genre",
                    op: "is",
                    value: "Comedy",
                  },
                ],
              },
            ],
            sort: {
              field: "added_at",
              order: "desc",
            },
          },
          sort_config: {},
          include_in_server_collections: false,
        },
      },
    ],
    "a limit of 250": [
      {
        operation: "PATCH /api/v2/collections/{id}",
        path: "/api/v2/collections/c1",
        headers: {
          "If-Match": '"/api/v2/collections/c1#1"',
        },
        body: {
          name: "Rainy days",
          is_shared: false,
          query_definition: {
            library_ids: [1],
            match: "all",
            groups: [
              {
                match: "all",
                rules: [
                  {
                    field: "genre",
                    op: "is",
                    value: "Comedy",
                  },
                ],
              },
            ],
            sort: {
              field: "added_at",
              order: "desc",
            },
            limit: 250,
          },
          sort_config: {},
          include_in_server_collections: false,
        },
      },
    ],
  } satisfies ByLimit,
  /** Personal create: the new collection form starts as Smart; the poster file uploads after the POST. */
  personalSmartCreate: [
    {
      operation: "POST /api/v2/collections",
      path: "/api/v2/collections",
      headers: {},
      body: {
        name: "Comfort",
        collection_type: "smart",
        is_shared: false,
        query_definition: {
          library_ids: [],
          match: "all",
          groups: [],
          sort: {
            field: "added_at",
            order: "desc",
          },
        },
        sort_config: {},
        include_in_server_collections: false,
      },
    },
    {
      operation: "PUT /api/v2/collections/{id}/poster",
      path: "/api/v2/collections/c1/poster",
      headers: {},
      form: {
        poster: {
          file: "poster.png",
        },
      },
    },
  ] satisfies Writes,
  /** Personal manual create: a pasted poster URL rides in the POST body, and no `description` is sent. */
  personalManualCreate: [
    {
      operation: "POST /api/v2/collections",
      path: "/api/v2/collections",
      headers: {},
      body: {
        name: "Rainy days",
        collection_type: "manual",
        is_shared: false,
        include_in_server_collections: false,
        poster_source_url: "https://images.example/poster.png",
      },
    },
  ] satisfies Writes,
  /** Personal manual update: no `description`. */
  personalManualUpdate: [
    {
      operation: "PATCH /api/v2/collections/{id}",
      path: "/api/v2/collections/c1",
      headers: {
        "If-Match": '"/api/v2/collections/c1#1"',
      },
      body: {
        name: "Renamed",
        is_shared: false,
        include_in_server_collections: false,
      },
    },
  ] satisfies Writes,
  /** Personal poster removal is sent the moment it is clicked, before Save. */
  personalPosterRemoval: [
    {
      operation: "DELETE /api/v2/collections/{id}/image",
      path: "/api/v2/collections/c1/image",
      headers: {},
      query: {
        type: "poster",
      },
    },
  ] satisfies Writes,
  /** Personal manual page: adding a title moves the collection's ETag, and the rename's PATCH still sends the one the page loaded with, so the server answers 412. */
  personalAddThenRename: [
    {
      operation: "PUT /api/v2/collections/{id}/items/{item_id}",
      path: "/api/v2/collections/c1/items/movie:alien-1979",
      headers: {},
      body: {
        position: 0,
      },
    },
    {
      operation: "PATCH /api/v2/collections/{id}",
      path: "/api/v2/collections/c1",
      headers: {
        "If-Match": '"/api/v2/collections/c1#1"',
      },
      body: {
        name: "Renamed",
        is_shared: false,
        include_in_server_collections: false,
      },
    },
  ] satisfies Writes,
  /** Admin MDBList import: `featured` defaults on. */
  adminImportMDBList: [
    {
      operation: "POST /api/v2/admin/collections/import/mdblist",
      path: "/api/v2/admin/collections/import/mdblist",
      headers: {},
      body: {
        title: "Top Watched",
        description: "",
        url: "https://mdblist.com/lists/user/top-watched/json",
        featured: true,
        sort_config: {},
        library_ids: ["1"],
      },
    },
  ] satisfies Writes,
  /** Admin TMDB chart import with the form's defaults. */
  adminImportTMDBChart: [
    {
      operation: "POST /api/v2/admin/collections/import/tmdb",
      path: "/api/v2/admin/collections/import/tmdb",
      headers: {},
      body: {
        title: "Trending Today",
        description: "",
        preset: "trending",
        time_window: "day",
        media_type: "all",
        featured: true,
        sort_config: {},
        library_ids: ["1"],
      },
    },
  ] satisfies Writes,
  /** Admin TMDB list import; the URL is sent as typed. */
  adminImportTMDBList: [
    {
      operation: "POST /api/v2/admin/collections/import/tmdb-list",
      path: "/api/v2/admin/collections/import/tmdb-list",
      headers: {},
      body: {
        title: "Festival Picks",
        description: "",
        url: "https://www.themoviedb.org/list/310-festival-picks",
        featured: true,
        sort_config: {},
        library_ids: ["1"],
      },
    },
  ] satisfies Writes,
  /** Admin template import: the template's server poster is sent as `poster_url`, and `featured` defaults on. */
  adminTemplateTMDB: [
    {
      operation: "POST /api/v2/admin/collections/import/tmdb",
      path: "/api/v2/admin/collections/import/tmdb",
      headers: {},
      body: {
        title: "Trending Movies This Week",
        description: "Top trending movies on TMDB.",
        featured: true,
        sync_schedule: "0 4 * * *",
        limit: 50,
        sort_config: {},
        poster_url: "https://images.example/templates/trending-movies.jpg",
        preset: "trending",
        media_type: "movie",
        time_window: "week",
        library_ids: ["1"],
      },
    },
  ] satisfies Writes,
  /** Admin template import of a list picked in MDBList search: the list's JSON URL and name. */
  adminTemplateMDBListPick: [
    {
      operation: "POST /api/v2/admin/collections/import/mdblist",
      path: "/api/v2/admin/collections/import/mdblist",
      headers: {},
      body: {
        title: "Oscar Winners",
        description: "Any public MDBList list.",
        featured: true,
        sort_config: {},
        url: "https://mdblist.com/lists/cinephile/oscar-winners/json",
        library_ids: ["1"],
      },
    },
  ] satisfies Writes,
  /** Personal template import: the cron default maps to a named schedule; the server poster is `poster_url`. */
  personalTemplateTMDB: [
    {
      operation: "POST /api/v2/collections/import/tmdb",
      path: "/api/v2/collections/import/tmdb",
      headers: {},
      body: {
        title: "Trending Movies This Week",
        description: "Top trending movies on TMDB.",
        sync_schedule: "daily",
        limit: 50,
        is_shared: false,
        poster_url: "https://images.example/templates/trending-movies.jpg",
        sort_config: {},
        preset: "trending",
        media_type: "movie",
        time_window: "week",
      },
    },
  ] satisfies Writes,
  /** Admin source editor, MDBList: the whole `source_config` is rebuilt and sent. */
  adminEditMDBList: [
    {
      operation: "PATCH /api/v2/admin/collections/{id}",
      path: "/api/v2/admin/collections/c1",
      headers: {
        "If-Match": '"/api/v2/admin/collections/c1#1"',
      },
      body: {
        title: "Original",
        description: "",
        featured: false,
        visibility: "visible",
        collection_type: "mdblist",
        source_url: "https://mdblist.com/lists/user/top-watched/json",
        source_config: {
          mode: "mdblist_json",
          url: "https://mdblist.com/lists/user/top-watched/json",
          limit: 100,
        },
        sync_schedule: "",
        sort_config: {},
        library_ids: ["1"],
      },
    },
  ] satisfies Writes,
  /** Admin source editor, TMDB chart: the whole `source_config` is rebuilt and sent. */
  adminEditTMDBChart: [
    {
      operation: "PATCH /api/v2/admin/collections/{id}",
      path: "/api/v2/admin/collections/c1",
      headers: {
        "If-Match": '"/api/v2/admin/collections/c1#1"',
      },
      body: {
        title: "Trending This Week",
        description: "",
        featured: false,
        visibility: "visible",
        collection_type: "tmdb",
        source_url: "tmdb://trending/movie/week",
        source_config: {
          mode: "tmdb_preset",
          preset: "trending",
          media_type: "movie",
          time_window: "week",
          limit: 40,
        },
        sync_schedule: "",
        sort_config: {},
        library_ids: ["1"],
      },
    },
  ] satisfies Writes,
  /** Admin source editor, TMDB list: the whole `source_config` is rebuilt and sent. */
  adminEditTMDBList: [
    {
      operation: "PATCH /api/v2/admin/collections/{id}",
      path: "/api/v2/admin/collections/c1",
      headers: {
        "If-Match": '"/api/v2/admin/collections/c1#1"',
      },
      body: {
        title: "Festival Picks",
        description: "",
        featured: false,
        visibility: "visible",
        collection_type: "tmdb",
        source_url: "https://www.themoviedb.org/list/310",
        source_config: {
          mode: "tmdb_list",
          url: "https://www.themoviedb.org/list/310",
        },
        sync_schedule: "",
        sort_config: {},
        library_ids: ["1"],
      },
    },
  ] satisfies Writes,
  /** Admin source editor, legacy Trakt: the stored `source_config` goes back unchanged and no `source_url` is sent. */
  adminEditTrakt: [
    {
      operation: "PATCH /api/v2/admin/collections/{id}",
      path: "/api/v2/admin/collections/c1",
      headers: {
        "If-Match": '"/api/v2/admin/collections/c1#1"',
      },
      body: {
        title: "For you",
        description: "",
        featured: false,
        visibility: "visible",
        collection_type: "trakt",
        source_config: {
          mode: "trakt_preset",
          preset: "recommended",
          media_type: "movie",
          profile_id: "p-owner",
          limit: 40,
        },
        sync_schedule: "",
        sort_config: {},
        library_ids: ["1"],
      },
    },
  ] satisfies Writes,
  /** Personal synced-list editor: optional fields go only when changed; name, sharing, libraries and the tab switch always go. */
  personalSyncedRename: [
    {
      operation: "PATCH /api/v2/collections/{id}",
      path: "/api/v2/collections/c1",
      headers: {
        "If-Match": '"/api/v2/collections/c1#1"',
      },
      body: {
        name: "Top Watched",
        is_shared: false,
        library_ids: ["1"],
        include_in_server_collections: false,
      },
    },
  ] satisfies Writes,
  /** Personal synced-list editor: clearing Max items sends `max_items: 0`. */
  personalSyncedClearLimit: [
    {
      operation: "PATCH /api/v2/collections/{id}",
      path: "/api/v2/collections/c1",
      headers: {
        "If-Match": '"/api/v2/collections/c1#1"',
      },
      body: {
        name: "Rainy days",
        is_shared: false,
        library_ids: ["1"],
        include_in_server_collections: false,
        max_items: 0,
      },
    },
  ] satisfies Writes,
  /** Bundle preview with the default hero sections. */
  bundleDryRunWithHeroes: [
    {
      operation: "POST /api/v2/admin/collections/template-bundles/{bundle_id}/apply",
      path: "/api/v2/admin/collections/template-bundles/core_defaults/apply",
      headers: {},
      body: {
        library_ids: ["1", "2"],
        dry_run: true,
        delete_existing: false,
        featured: {
          home: {
            library_id: "1",
            template_id: "tmdb_trending_movies_week",
          },
          libraries: {
            "1": "tmdb_trending_movies_week",
            "2": "tmdb_trending_tv_week",
          },
        },
      },
    },
  ] satisfies Writes,
  /** Bundle preview with every hero off and Delete Existing on: no `featured` member. */
  bundleDryRunNoHeroesDeleteExisting: [
    {
      operation: "POST /api/v2/admin/collections/template-bundles/{bundle_id}/apply",
      path: "/api/v2/admin/collections/template-bundles/core_defaults/apply",
      headers: {},
      body: {
        library_ids: ["1", "2"],
        dry_run: true,
        delete_existing: true,
      },
    },
  ] satisfies Writes,
  /** Bundle apply job with the default hero sections. */
  bundleJobWithHeroes: [
    {
      operation: "POST /api/v2/admin/collections/template-bundles/{bundle_id}/apply-job",
      path: "/api/v2/admin/collections/template-bundles/core_defaults/apply-job",
      headers: {},
      body: {
        library_ids: ["1", "2"],
        delete_existing: false,
        featured: {
          home: {
            library_id: "1",
            template_id: "tmdb_trending_movies_week",
          },
          libraries: {
            "1": "tmdb_trending_movies_week",
            "2": "tmdb_trending_tv_week",
          },
        },
      },
    },
  ] satisfies Writes,
  /** Bundle apply job with every hero off: no `featured` member. */
  bundleJobNoHeroes: [
    {
      operation: "POST /api/v2/admin/collections/template-bundles/{bundle_id}/apply-job",
      path: "/api/v2/admin/collections/template-bundles/core_defaults/apply-job",
      headers: {},
      body: {
        library_ids: ["1", "2"],
        delete_existing: false,
      },
    },
  ] satisfies Writes,
  /**
   * Bundle apply job with the default hero sections and Delete Existing on.
   * This is the request that deletes server collections; today it is sent
   * with no confirmation step.
   */
  bundleJobDeleteExisting: [
    {
      operation: "POST /api/v2/admin/collections/template-bundles/{bundle_id}/apply-job",
      path: "/api/v2/admin/collections/template-bundles/core_defaults/apply-job",
      headers: {},
      body: {
        library_ids: ["1", "2"],
        delete_existing: true,
        featured: {
          home: {
            library_id: "1",
            template_id: "tmdb_trending_movies_week",
          },
          libraries: {
            "1": "tmdb_trending_movies_week",
            "2": "tmdb_trending_tv_week",
          },
        },
      },
    },
  ] satisfies Writes,
  /** Add to collection, own manual collection: the personal item route. */
  addToPersonalCollection: [
    {
      operation: "PUT /api/v2/collections/{id}/items/{item_id}",
      path: "/api/v2/collections/c1/items/movie:heat-1995",
      headers: {},
      body: {
        position: 0,
      },
    },
  ] satisfies Writes,
  /** Add to collection as an acting admin, server manual collection: the admin item route. */
  addToServerCollection: [
    {
      operation: "PUT /api/v2/admin/collections/{id}/items/{item_id}",
      path: "/api/v2/admin/collections/lc1/items/movie:heat-1995",
      headers: {},
      body: {
        position: 0,
      },
    },
  ] satisfies Writes,
  /** What Add to collection lists, by group. */
  addToCollectionGroups: {
    profile: [{ group: "My Collections", collections: ["Rainy days"] }],
    actingAdmin: [
      { group: "My Collections", collections: ["Rainy days"] },
      { group: "Movies", collections: ["Oscar Winners · Library"] },
    ],
  },
  /** What Add to collection reads to fill its list. */
  addToCollectionReads: {
    profile: ["GET /api/v2/collections"],
    actingAdmin: ["GET /api/v2/collections", "GET /api/v2/library/{id}/collections"],
  },
};
