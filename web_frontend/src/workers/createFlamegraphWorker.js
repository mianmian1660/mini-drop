// Keep the bundler-specific import.meta expression out of page modules so the
// lifecycle UI can be exercised by Jest without constructing a real Worker.
export default function createFlamegraphWorker() {
    return new Worker(new URL('./flamegraphWorker.js', import.meta.url));
}
