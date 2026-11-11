export const store = document.body.dataset.profile === 'store';
export const cameras = store ? [
  { id: 'dining_area', name: 'Dining Area' },
  { id: 'cash_register', name: 'Cash Register' },
  { id: 'kitchen_back', name: 'Kitchen Back' },
] : [{ id: 'home_camera', name: 'Home test camera' }];
export const playbackOrigin = 'http://127.0.0.1:' + (store ? 19996 : 9996);
export let camera = cameras[0];

function select(id) {
  const next = cameras.find(item => item.id === id);
  if (!next || next === camera) return;
  camera = next;
  document.getElementById('playback-camera').value = id;
  updateCamera();
  document.dispatchEvent(new Event('cctv-camera'));
}

function updateCamera() {
  document.getElementById('camera-name').textContent = camera.name;
  document.querySelector('.camera-title .channel').textContent = 'CCTV 0' + (cameras.indexOf(camera) + 1);
  document.querySelector('.monitor-rail h2').textContent = camera.name;
  for (const link of document.querySelectorAll('[data-camera-link]')) {
    if (link.dataset.cameraLink === camera.id) link.setAttribute('aria-current', 'true');
    else link.removeAttribute('aria-current');
  }
}

export function focusCamera(id) {
  select(id);
  location.hash = 'live';
}

if (store) {
  const picker = document.getElementById('playback-camera');
  picker.parentElement.hidden = false;
  document.querySelector('.workspace-bar .eyebrow').textContent = 'Store workspace';
  document.querySelector('.sidebar summary').lastChild.textContent = ' Store workspace';
  document.querySelector('.sidebar .count').textContent = cameras.length;
  document.querySelector('.sidebar-footer small').textContent = 'Bounded store archive';
  document.querySelector('#storage-view .metrics strong').textContent = 'Bounded run';
  document.querySelector('#storage-view code').textContent = 'local/store-dashboard/';
  const original = document.querySelector('.camera-link');
  cameras.forEach(({ id, name }, i) => {
    const link = original.cloneNode(true);
    link.dataset.cameraLink = id;
    const small = link.querySelector('small');
    link.querySelector('span').firstChild.textContent = name;
    small.textContent = 'CCTV 0' + (i + 1);
    link.addEventListener('click', () => select(id));
    original.before(link);
    picker.add(new Option(name, id));
  });
  original.remove();
  picker.addEventListener('change', () => select(picker.value));
  const zoom = document.getElementById('timeline-zoom');
  zoom.prepend(new Option('2 minutes', String(2 / 60)));
  zoom.value = String(2 / 60);
  updateCamera();
}
