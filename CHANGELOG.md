# Changelog

What changed in each release of q2-subsonic, the Subsonic app for Q2 Pod, newest first. Version X.Y is the release tag `q2-vX.Y`; Q2 Pod's App Store installs the latest one. The format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/).

A few features need a newer Q2 Pod. Each one says so, and what the app does on older Q2 Pod instead.

## [Unreleased]

### Added

- Lyrics from your server: when the Q2's lyrics page shows a song from your server that has no lyrics of its own, the app asks the server for them once (OpenSubsonic's `getLyricsBySongId` on Navidrome, else `getLyrics` by artist and title) and saves them beside the song as a `.lrc`, timed line by line when the server's are, so the page shows them, and shows them at once the next time with no request. A song the server has none for isn't asked about again for 30 days. The `.lrc` goes when its song leaves the card or the Stream Cache. Requires a Q2 Muse whose lyrics page asks the app (q2app's `lyrics=1`); older firmware never asks.
- Album covers in the car without the wait: when the car shows an album from your server whose cover the Q2 doesn't have yet, the app fetches that cover from the server right away, at the car's size, and keeps it with the others, instead of the car showing a blank tile until the app's background rounds reach the album. Each cover is asked for once; one the server has none of isn't asked again, and one that couldn't be fetched (the server out of reach, the card full) is tried again after 10 minutes at the earliest. Requires a Q2 Muse whose Car Remote asks the app (q2app's `covers=1`); older firmware never asks.
- The Q2 says when your server can't be reached: when a request finds no answer, a name that doesn't resolve, or a proxy answering 502, 503 or 504 for a server that is down, the app tells the Q2, which toasts why ("Server error 502", "Server not answering", "Server not found") and shows a crossed-out cloud in its status row until a request to the server succeeds again. A song from your server that is skipped or stops the queue for a server error toasts why too, rather than the Q2 skipping it with nothing on screen. Requires a Q2 Muse that reads the app's `reach=` and `srverr=` keys; older firmware shows nothing new.
- A streamed queue picks up where it left off after the Q2 restarts: when Memory Playback brings back a queue of songs from your server, the app streams it again from the song that was playing, paused as the Q2 left it, and downloads the songs after it as usual, so Up Next keeps the whole queue instead of only the one or two songs already on the card. Requires a Q2 Muse that asks the app (q2app's `resume=1`); older firmware restores only the songs on the card, as before.

### Changed

- The app no longer counts its folder's size every 10 minutes while it runs (several seconds of the Q2's CPU each time, with thousands of covers): it counts it when it starts, when the Storage page opens, and after it wrote there. With a Q2 Muse that doesn't say when the page opens, the page shows the size counted at the app's start or after its last write.
- **Settings → Diagnostics** lists what went wrong and the syncs, newest first, kept for a week: a song skipped or that couldn't stream, the server going out of reach, a sync that finished or stopped, each with why and when. A row opens its **Details** in plain words (what, while doing what, why, when, the song, the server, and a sentence on what it means) instead of "Op: stream". The first page no longer shows a loose "Skipped …" row.
- A song that can't be fetched because the server is out of reach, or a proxy answers 502, 503 or 504 for it, is waited for instead of skipped, so Now Playing can say why and offer Retry and Skip; a list played from the Q2's Library no longer drops its songs that aren't on the card.
- The app's log says when its background jobs start and how long they take (the storage walk, the server library's checks).

- **Sync** on a Q2 Muse that draws it is one centred screen per state, as a phone's software update: Checking for changes, Up to date, N new songs with **Sync Now**, Syncing with a progress bar and **Stop**, Couldn't reach the server with **Try Again**, Nothing to sync yet. It asks the server every time it opens, even with nothing chosen, so a server that is down is no longer reported as up to date. Requires a Q2 Muse with the firmware's `status` word; older firmware keeps the rows.

- Sync Offline from the Q2's Library: hold **Play/Pause** on a server song, album, artist or playlist, or open Now Playing's ⋯, and choose **Sync Offline** (or **Stop Syncing Offline**); the app chooses it for the next Sync and the Q2 says so. Requires a Q2 Muse with the Sync Offline row (q2app's `offline=1`).
- Sync's wording is the same everywhere: **Sync Offline** and **Stop Syncing Offline** (were Add to Sync and Remove from Sync), **Sync Entire Library Offline**, the toasts `Syncing offline · N songs`, `Stopped syncing offline` and `Offline via <name>` (were Added to Sync, Removed from Sync and Included via), and the statuses `Available offline` and `X of Y songs offline` (were On Q2 and X of Y songs on Q2).

- The app's first page is now only its own: **Downloads** (what was **On This Q2**, with **Sync Entire Library** at its top), **Sync** and **Settings**, each with a line saying where it stands. Its Playlists, Albums, Artists, Discover and Search pages and the five swipe tabs are gone: while you are signed in, your server's music is in the Q2's own Library. To choose single albums or artists to sync, use **Go to Album** or **Go to Artist** on a song from the server.
- The server's songs are in the Q2's Library whenever you are signed in: the **Show in Library** setting is gone, and signing out takes them out.

- Stars and likes are separate: a song's stars on the Q2 go to your server as its rating, and a like (the heart) as its star, each on its own, so liking a song no longer sets five stars and rating a song no longer takes its like away. Both come back from the server the same way. Requires a Q2 Muse that keeps likes apart from stars (the firmware's `likes` extension; q2app now says `ratings=2`); on older firmware stars and likes stay on the Q2 until it is updated.

### Fixed

- A radio or a streamed queue no longer stops after the songs already downloaded when the Q2 plays with Shuffle and Repeat All: once every song on the card had played, the Q2 named those songs again as the next to play, they took Download Ahead's places, and the next song was never downloaded, so Next stopped on it with a spinner for good. Songs already downloaded and played no longer count against Download Ahead. With the Stream Cache off, those songs were also deleted and downloaded again many times a second; the songs the Q2 names to come are now kept.
- Radio started again on the same song plays: stopping a song's radio and starting it again on that song (from the car, or on the Q2) said "Radio isn't available for this song", because every similar song the server named had just been queued by the first radio. A radio started again now plays them again.
- Starting Radio on the song that is playing no longer downloads that song a second time: the radio plays on from the file the app already has, or waits for the one still arriving, instead of fetching it again, which wasted data, card writes and CPU.
- With Show in Library, the app no longer takes a burst of the Q2's CPU every minute while music plays: once every album cover it can fetch is on the card, it stops checking them until your library or the card changes, and a covers folder that is full no longer downloads a cover a minute only to throw it away. The car's 512 pixel covers are saved as the server sends them when they are already a plain JPEG, instead of being decoded and encoded again, which took most of the CPU while they download. The app also runs at a lower priority, so the Q2's player, Bluetooth and screens go first.

### Security

- Built with Go 1.27.4 instead of 1.22.2, which brings the app's copy of the Go standard library (TLS, HTTP, image and archive handling) up to date: a vulnerability scan of the app found 77 known issues in the old one and none in the new. The app is about 1.9 MB larger on the card (the zip grows from 2.9 MB to 3.6 MB).
- The app's connections to your server follow Go 1.27's stricter defaults: a server that signs its TLS handshake with SHA-1, uses an RSA key under 1024 bits, or has a certificate with a negative serial number is refused, and the handshake offers post-quantum key exchange (X25519MLKEM768) where the server supports it. Servers with a certificate from a public authority such as Let's Encrypt are unaffected.

## [1.0] - 2026-11-16

### Added

- Song radio: Now Playing's **Radio** on a song from your server starts an endless radio of songs like it (`getSimilarSongs2` with the song's id), the song playing on where it is. More songs are added when about 10 are left, and none played or queued lately (the last 200) comes back; the Q2's blue **Radio** pill lights while it plays, and a tap on it stops it, the queued songs playing on. An artist's **Radio** is endless the same way. Now Playing's Radio requires a Q2 Pod with the `radio` extension; an artist's Radio is endless on any.
- The Q2's status bar shows when the app is busy with your server: turning arrows while it checks the server or your library, and a ring filling up while a sync downloads songs. Requires a Q2 Pod that shows it (its status row's sync indicator); an older one ignores it.
- **Download Ahead:** while a list streams, the app downloads the songs the Q2 will play next, in the Q2's own order (shuffle and Up Next included), so a skip or the next song starts at once. How many (Off, 1, 2 or 3; 2 by default) is the Q2's **Streaming → Configuration → Download Ahead**; a song that leaves the plan while it downloads is dropped and the new next song fetched. The app also tells the Q2 how far each song has downloaded, which Now Playing shows while a song loads, and on a Q2 Muse whose player can play a song as it arrives, a song starts after a few seconds of it rather than once all of it is on the card. Requires a Q2 Muse with the `ahead` extension (and `grow` for the early start); on older firmware the app downloads the next two songs of the list, as before.
- **Retry** on the Q2's Now Playing, when a song can't load because your server is out of reach, makes the app try that song again at once instead of at its next scheduled try. Requires a Q2 Muse that shows Retry; older firmware never asks.
- With Show in Library, album covers in the car are sharp: once the Q2 has every album's cover, the app also fetches each at 512 pixels (into `.subsonic/covers/512/`, at most 256 MB), which the Q2 Muse's Car Remote sends to the car instead of the small one. Requires a Q2 Muse whose Car Remote sends them; an older one keeps sending the small covers.

### Fixed

- With Show in Library, a server playlist's cover on the Q2 follows the playlist: when its songs change and Navidrome makes it a new cover, the app fetches the new one instead of keeping the first, and a playlist that had no cover gets one once the server makes it.

## [0.9] - 2026-11-16

### Added

- Sign in from your phone, redesigned: one press opens the QR code; scanning it opens the form with no PIN to type (an address typed by hand asks for the PIN first). The page is dark with grouped fields, says what went wrong under the field it concerns (an `http://` address, a server the Q2 can't reach, one only on Tailscale), cuts a pasted `/rest/…` path off the address, and fills in the last server you signed in to. The Q2 follows the phone: **Continue on Your Phone**, **Signing In…**, **Signed In**, or why it failed. Requires a Q2 Pod with the `signin` extension for the one press and the Q2's pages; on older Q2 Pod the row takes two presses and the QR page shows the outcome beside the code, as before.
- **At Home** on the phone sign-in page: your server's address on your home network (such as `http://192.168.1.20:4533`), which the Q2 asks first while its Wi-Fi is on that subnet, and the server address everywhere else or when home doesn't answer. Kept with the settings; **Settings → Account** shows it.
- A server on your home network can be signed in to over plain `http://` when its address is a private one (`10.x`, `172.16` to `172.31`, `192.168.x`). `http://` anywhere else is still refused, and the sign-in page says why under the field.
- **Settings → Show in Library** puts your server's songs and playlists in the Q2's own Library, beside the songs on the card, with the Q2's **Library → Local + Server**. Songs you synced show once. A server song plays as the app's lists do, from the Stream Cache when it is there; if the server is lost while a list plays, the songs not on the card are left out and the rest play on. The first time, the app asks for every song, 500 at a time, with no limit on the library's size; later it asks again only when the server says its library changed, once the Q2 is idle or charging, or when you press **Settings → Resync Library**. A song the server no longer has stays while the Stream Cache keeps it. Off by default. Requires a Q2 Pod with the server library (the `catalogue` extension); on older Q2 Pod the row is not there and nothing changes.
- Show in Library follows the Q2's **Settings → Library → Streaming Songs** switch: it says On only while Streaming Songs is on, and turning it on while Streaming Songs is off asks to turn Streaming Songs on too. While Streaming Songs is off the app asks the server nothing for the Library and keeps what it has, so turning Streaming Songs on again goes on without a new first copy. Requires a Q2 Pod with the `libsource` extension; on older Q2 Pod Show in Library is On or Off by its own setting, as before.
- With Show in Library, the server's albums show their covers in the Q2's Library. The app fetches them in the background, a few at a time, while no sync runs and no streamed song is downloading (a song playing from the Stream Cache doesn't stop it) or while the Q2 charges, into `.subsonic/covers` (at most 256 MB of the card, none written below 1 GB free). Requires a Q2 Pod that shows them (the server library's covers); an older one leaves the folder unread.
- The Q2's **System settings → Storage** shows what the app takes of the card: **Downloads** (your synced songs in `Subsonic/`) and **Stream Cache** (all of `.subsonic/`: the cache, covers and the app's files), and under Downloads each item you chose to sync with its size. Press one to open it here; hold Play/Pause on it to take it out of Sync, as On This Q2 does. Its **Limit** (No Limit, the default, or 4 to 64 GB) caps your downloads: a sync that would go past it says `Over the 16 GB Downloads limit · Take out 1.2 GB` and offers no Start. The app reads nothing outside `Subsonic/` and `.subsonic/` to count this; your own music is measured by the Q2. Requires a Q2 Pod with the `storage` extension; on older Q2 Pod nothing is written and there is no limit.
- Hearts given to a song on the Q2 become its rating on the server, and five hearts star it; ratings and stars changed on the server reach the Q2. A change made while the server is out of reach is sent when it answers again. Requires a Q2 Pod with the `ratings` extension and Show in Library on; on older Q2 Pod hearts stay on the Q2.
- An artist's page says how many albums, singles and songs the artist has, and its genre, and has **Singles & EPs**, **Similar Artists** (each opens that artist's page) and **All Songs**, each only when there is something in it. Top Songs and All Songs show each song's album and length, and Top Songs is left out when the server has none. Singles and EPs are told apart by the server's release types (Navidrome 0.53 or later); a server without them shows every release under the albums. Requires a Q2 Pod with the `artist` extension; on older Q2 Pod the page is as before.
- With Show in Library, a server playlist's cover from the server shows on its row in the Q2's Playlists, fetched as the albums' covers are. Requires a Q2 Pod that shows it (the `playlists` extension's Playlists rows under OLED); an older one leaves the file unread.
- With Show in Library, a server playlist you change on the Q2 is changed on the server too: songs added or removed, a new name, or the playlist deleted. The app sends each change while the server answers (a playlist's songs as they now stand, in order), and keeps what it couldn't send for the next connection. Songs on the card that the server doesn't have are left out of the server's copy. Requires a Q2 Pod with the `playlists` extension; on older Q2 Pod a playlist changed on the Q2 is put back as the server has it at the next sync.
- An album's page has **Shuffle** (the album's songs in a random order) beside Play, says the album's artist, year, number of songs and running time (written as the Q2 writes time, such as 1h12m or 45 min), and shows each song's length. Requires a Q2 Pod with the `album` extension, which draws it with the album's cover; on older Q2 Pod the page is as before.

### Fixed

- A sync's time left is written as the Q2 writes time: **About 1h12m left**, **About 45 min left**.
- The phone sign-in no longer stops when the Q2's screen turns off before the code is scanned (with Q2 Pod's `signin` extension: the app then knows the QR page was left from the firmware, not from a pause in the page's refreshes).
- A link from the QR code opened from a phone's camera no longer lands on the PIN step: the page's cookie was set on a redirect, which a cross-site visit doesn't carry.

### Security

- `.subsonic/log.txt` no longer names the server user of each request: the user name is hidden as the password, token and API key already were. At start the app hides it in what an older version left in `log.txt` and `log.txt.1`.
## [0.8] - 2026-11-14

### Added

- **Playlist art.** A sync puts each synced playlist's picture from the server beside its playlist on the card, where Q2 Pod shows it in Playlists (Row Art in Classic, and Cover Flow): your own picture when the playlist has one, else the server's mix of its album covers. A changed picture is fetched at the next sync, and a picture you put there yourself under the same name is left alone. Requires Q2 Pod 1.8.1 or later; older Q2 Pod ignores the pictures.

## [0.7] - 2026-11-12

### Fixed

- A song whose file changed on the server without a tag the app names it by (a genre, comment or cover edit, or a re-rip) is downloaded again once. Before, every sync offered to replace it and never did.

## [0.6] - 2026-11-12

### Added

- **Discover**, a new tab of the first page (a row of its list on older Q2 Pod): **Shuffle All**, **Recently Added**, **Recently Played**, **Most Played**, **Random Albums**, **Starred** and **Genres**. A genre lists its albums and has its own **Shuffle**. Recently Played and Most Played show what the server user has played, songs played from the app included (see Report Plays below).
- An artist's page has **Play**, **Shuffle**, **Radio** (songs like the artist's) and **Top Songs**. On a server without Radio or Top Songs (Navidrome has both, through Last.fm) they say **Not available on this server**.
- A long list no longer ends in **More…** after 500 rows. It ends in **Next page**, which shows the next part in its place, with **Previous page** to go back, and Back leaves the list at once. Requires Q2 Pod 1.7.3 or later; on older Q2 Pod long lists keep More…, as before.
- **Artists** opens on the server's index (the letters A to Z on Navidrome) when you have more than 60 artists, each letter with its count. Requires Q2 Pod 1.7.3 or later; on older Q2 Pod, or a server that names no index, Artists stays one list.
- **Albums** has a **Sort** row: **Recently Added**, **By Artist** or **By Year** (newest first). The order is not remembered: Albums opens A to Z. Requires Q2 Pod 1.7.6 or later; on older Q2 Pod Albums is A to Z only.
- **Albums**, **Playlists**, Discover's album lists and a genre's albums show each one's cover beside its row. Pages that mix albums with other rows, such as an artist's page, Search and On This Q2, keep their plain rows. The list opens at once and its covers appear a few seconds later; in a long list, covers past the first rows appear the next time you open it. Covers come slower while a sync runs. Requires Q2 Pod 1.7.3 or later; on older Q2 Pod the lists look as before. A server that sends no cover, or one in WebP, leaves the rows plain.
- Songs you play from the app are reported to your server. It shows the song as playing now, and counts a play once the song has been on for half its length or 4 minutes, so play counts and a Last.fm or ListenBrainz account linked on the server follow what you hear on the Q2. A play that can't be sent is tried again with the next song. **Settings → Report Plays** turns it off. A song paused early still counts, as the Q2 doesn't tell the app about a pause. Songs played from the Q2's own Library are not reported. If the Q2 also uploads its own scrobbles to the same account, turn one of the two off.
- Songs you stream are kept on the card, in a stream cache, so a song streamed before starts at once, and a list you started plays on without Wi-Fi as far as its songs are kept. **Settings → Stream Cache** sets its size: 512 MB, 1 GB, 2 GB (the default), 4 GB, 8 GB or **Off**; each press takes the next one. When it is full, the songs played longest ago go first, and it gives way until 256 MB of the card are free. **Settings → Clear Cache** shows how much is kept and empties it on a second press; the list that is playing stays. A sync comes first: it takes the cache's space when it needs it, and the Sync page says so (`Takes 1.2 GB from the Stream Cache`). A kept song belongs to the quality it was streamed in, so another **Streaming Quality** fetches it again, and **Sign Out** empties the cache, but for a list that is still playing. Needs the firmware's `playlist` extension; without it, and with Stream Cache off, streamed files are deleted as you go, as before.
- **Go to Album** and **Go to Artist**, in Now Playing's hold menu, work on a song you stream: they open that album or artist in the app, where you can play it or add it to your sync. Requires Q2 Pod 1.6.0 or later; on older Q2 Pod nothing changes. Songs you have synced open the Library's own pages, as before.

### Changed

- The first page is now tabs: **Home** (the whole list as before, so everything is still reachable with the wheel), **Discover**, **Playlists**, **Albums** and **Artists**. Swipe left or right to move between them; the title says which one you are on, such as Albums 4/5. A skipped or failed song is reported on Home. Requires Q2 Pod 1.7.3 or later; on older Q2 Pod the first page is one list, as before.
- **Search** is a magnifying glass at the right of the title bar on every tab; tap it to search. Under **Theme: Classic**, which has no title bar, it is the first row of each tab. Requires Q2 Pod 1.7.3 or later; on older Q2 Pod Search stays a row in the list.
- A song pressed in search results plays on through the songs found, instead of alone.
- With **Sync Entire Library** on, the "Checking server…" step of Sync is faster. The app asks for the songs 500 at a time instead of album by album. When the server says its library hasn't changed since the last check, less than a day ago, only the playlists are checked again (Navidrome says so; the songs it found are kept in `.subsonic/library.json`, for a library of up to 20,000 songs). A server that can't do either is checked album by album, as before. An album that seems to have lost a song you have synced is asked for on its own before the song is removed from the card. **Check Again** always checks everything.
- While it checks, the Sync page counts, such as `4500 of 10000 songs`, then `3 of 12 playlists`. Once the server has answered it says `Working out the changes…` while it compares them with the card, which takes a while with a large library.
- The check no longer gives up after 10 minutes on a large library. It stops when the server stops answering, or takes more than 2 minutes over one answer.
- The check looks through the card for songs deleted from it once each time the app starts and when you press **Check Again**, not at every check: on a large library that look took most of the time. A song you delete on the Q2 while the app runs is synced again after **Check Again** or the next start.
- Artists, Playlists, a long playlist or album and search results are fetched from the server once, not once for every 50 rows, so long lists fill faster. A song pressed on a page you just opened starts without fetching its list again. A change made on the server can take a minute to show.

### Fixed

- A streamed list no longer ends when Wi-Fi drops for a while. The songs already fetched play on, the app keeps trying the next one, and the list goes on when the server answers again. Before, the list ended about 40 seconds after the server was lost, even if Wi-Fi came back. It still ends if no song has started for 30 minutes. One song that fails on its own while the server still answers is not waited for: the list stops at it, as before.
- An internal error while answering one page no longer ends the app, and with it a running sync or stream. The page says "Something went wrong" with Retry; the error goes to `.subsonic/log.txt`.
- **Sign Out** removes the list of the server's songs that Sync keeps on the card (`.subsonic/library.json`). The songs you synced stay, as before.
- **Default Quality** and **Streaming Quality** say "Couldn't save the setting" when the card can't be written, instead of showing a quality that wasn't saved.
- Sync's **Last synced** is when the last sync finished. Before, a check of the server could move it, though nothing was synced. On a card synced with an older version it is right from the next sync on.
- Albums and artists show **On Q2**, or how many of their songs are on the Q2, as soon as the app starts. Before, Albums and Recently Added showed nothing for them until Sync had checked the server. Songs synced with an older version show it again after the next check.

## [0.5] - 2026-11-07

### Fixed

- A chosen album, artist, playlist or song that the server no longer has no longer stops the whole sync with "Album not found". This happens when the server's library is reorganised and the item gets a new id. The rest of your selection now syncs. The missing item reads **Not on the server** in On This Q2, and its songs stay on the Q2 until you remove it there. If the server can't be reached, the sync still stops, so songs are never removed because of a network problem.

## [0.4] - 2026-10-30

### Added

- **Settings** shows the app's version, to quote in a bug report.

### Fixed

- Stopping the app from the Q2 can no longer hang: if the app hasn't ended 2 s after it's asked to, the launcher kills it, so USB storage and the App Store don't wait on it.

## [0.3] - 2026-10-30

### Changed

- While you stream a list, a song the server can't send (gone from the server, or in a format it can't convert) is skipped and the songs after it play on. The main page says which song was skipped and why. If the server can't be reached or refuses your account, or three songs in a row fail, the stream still stops at that song. Requires Q2 Pod 1.5.9 or later; on older Q2 Pod the stream stops at such a song, as before.

## [0.2] - 2026-10-30

### Fixed

- A playlist whose name is longer than the 63 bytes the Q2 keeps no longer gets another copy on the Q2 at every sync: its file name is now cut to fit. The next sync moves such a playlist to the shorter name; copies an older version already left on the Q2 stay until you delete them. ([#1](https://github.com/andsuor/q2-subsonic/issues/1))

## [0.1] - 2026-10-29

First release. It needs Q2 Pod with card apps and a Subsonic or OpenSubsonic server (Navidrome, for one) reachable over HTTPS.

### Added

- Browse your server on the Q2 under **Streaming → Subsonic**: Playlists, Albums, Artists and Search.
- Long lists load in full. Requires Q2 Pod 1.5.9 or later; on older Q2 Pod a **More…** row at the end loads the next page.
- Press a song to play it and the rest of the list after it. Songs already synced play from the card, and the others stream in their own format when the Q2 plays it. Requires Q2 Pod 1.5.9 or later; on older Q2 Pod every song in the list streams, synced or not, and only FLAC and MP3 stream as they are, other formats as MP3 320.
- **Settings → Streaming Quality** streams the originals (the default) or converts every streamed song to MP3 320 or MP3 128.
- Sync playlists, albums, artists and single songs to the card over Wi-Fi: hold **Play/Pause** on one, or open it and press **Add to Sync**. **On This Q2** lists what you picked, with its status and size on the card.
- **Sync Entire Library**, the first row of the main list, syncs everything on the server.
- Every sync starts with a preview of the songs to add, remove and replace, the playlists to update, and the space it needs and has. Nothing happens until you press **Start Sync**, and a card that is too small gets no Start.
- Sync progress shows percent, songs, time left and the current song, with **Pause**, **Resume** and **Cancel**. The sync carries on when you leave the page, and picks up where it was after a Wi-Fi drop or a restart.
- A progress bar on the sync page. Requires Q2 Pod 1.5.9 or later; on older Q2 Pod it is text, such as `[#####-----] 47 %`.
- Songs that fail to sync are listed under **Failed Songs**, with **Retry Failed**.
- Quality per synced item: Original, MP3 320 or MP3 128, with a default in **Settings → Default Quality**. An original the Q2 can't play (WavPack, Musepack) syncs as MP3 320.
- Synced playlists keep their name, order and duplicates, and a synced artist includes albums added to the server later.
- After a sync the Q2 updates its library and imports the playlists by itself, waiting while music plays. A playlist you made on the Q2 is never replaced: a server playlist of the same name arrives with " (Subsonic)" after its name. Requires Q2 Pod 1.5.9 or later; on older Q2 Pod run **Library → Update Local Music** and **Playlists → Import playlist** yourself, as the sync page reminds you.
- Sign in from your phone: the Q2 shows a QR code and a PIN, and you enter the server address, user name and password on the phone.
- Sign in with your password, or with an OpenSubsonic API key in its place when the server offers them. With a key, only the key is stored on the card.
- **Settings → Account** shows the server and user, and **Sign Out** removes the stored login. Your music stays on the card.
- A failure shows a short message and a **Details** row with the operation, the HTTP status or Subsonic error code, and the time. The full trail is in `.subsonic/log.txt`, and if the app crashed, its trace is added there at the next start.

### Security

- Only `https://` servers, with certificates checked against a built-in list of trusted authorities. When the Q2's clock is unset, the time for that check comes from a server whose certificate checks out.
- The phone sign-in page opens a port only after you press **Sign In from Your Phone**. It needs the one-time key from the QR code or the PIN, locks for a minute after 10 wrong PINs in a row, and stops once you are signed in, when you leave the QR page, or after 10 minutes. The password crosses your Wi-Fi from the phone to the Q2 unencrypted, once: sign in on a network you trust.
- The password is never sent to the server: sign-in uses a salted token, or the API key.
- Passwords, tokens, API keys and a `user:password@` in the server address never reach `log.txt` or the Account page.
- The app changes only `Subsonic/`, the playlist files it wrote and `.subsonic/`, and deletes only what a sync put there, after the downloads have finished.

[Unreleased]: https://github.com/andsuor/q2-subsonic/compare/q2-v1.0...HEAD
[1.0]: https://github.com/andsuor/q2-subsonic/compare/q2-v0.9...q2-v1.0
[0.9]: https://github.com/andsuor/q2-subsonic/compare/q2-v0.8...q2-v0.9
[0.8]: https://github.com/andsuor/q2-subsonic/compare/q2-v0.7...q2-v0.8
[0.7]: https://github.com/andsuor/q2-subsonic/compare/q2-v0.6...q2-v0.7
[0.6]: https://github.com/andsuor/q2-subsonic/compare/q2-v0.5...q2-v0.6
[0.5]: https://github.com/andsuor/q2-subsonic/compare/q2-v0.4...q2-v0.5
[0.4]: https://github.com/andsuor/q2-subsonic/compare/q2-v0.3...q2-v0.4
[0.3]: https://github.com/andsuor/q2-subsonic/compare/q2-v0.2...q2-v0.3
[0.2]: https://github.com/andsuor/q2-subsonic/compare/q2-v0.1...q2-v0.2
[0.1]: https://github.com/andsuor/q2-subsonic/releases/tag/q2-v0.1
