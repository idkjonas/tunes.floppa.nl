const data = JSON.parse(document.getElementById("trackData").textContent);
console.log(data);

if ("mediaSession" in navigator) {
  navigator.mediaSession.metadata = new MediaMetadata({
    title: data.title,
    artist: data.user.username,
    album: "Soundcloak",
    artwork: [
      {
        src: data.artwork_url,
        sizes: "500x500",
        type: "image/jpeg",
      },
    ],
  });
}
