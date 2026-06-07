### server homepage

i wanted a one stop page to showcase all of my server stats, all port which are open, all urls which i have configured, what processes are taking up resources,etc.

i've seen open source alternatives like homepage here : [https://github.com/gethomepage/homepage](https://github.com/gethomepage/homepage), but eventually ended up building my own as it was very hard to customize and i had growing requirements which may be opinionated.

i liked go, so i built it with go. 1 executable. 1 compiled binary. 

i liked catpuccinmocha and jetbrains mono and lowercase, and so they are present here.

since this is gated behind my tailnet, i can't show you a live deployement ever, but below is a small demo of what it looks like to me:
[demo of my stats page](./assets/server-homepage.mp4)

### todo

- update with new services
- add a free port checker dialog box somehwere which suggests free port for a new deployment
- tracker for all of the cloudflare tunnels i have on my server, along with their stats, dig times, propagation routes and latency.
...more soon




**mirrored from local forgejo instance, not touching github with a 10-foot pole**
