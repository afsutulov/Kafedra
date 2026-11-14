(function () {
  'use strict';

  // Версия для слабовидящих: признак хранится в браузере пользователя.
  var root = document.documentElement;
  try { if (localStorage.getItem('a11y') === '1') root.classList.add('a11y'); } catch (e) {}
  var a11y = document.getElementById('a11y-toggle');
  if (a11y) a11y.addEventListener('click', function () {
    var on = root.classList.toggle('a11y');
    try { localStorage.setItem('a11y', on ? '1' : '0'); } catch (e) {}
  });

  // Кнопка «показать пароль» у каждого поля ввода пароля.
  var NS = 'http://www.w3.org/2000/svg';
  function eyeIcon() {
    var svg = document.createElementNS(NS, 'svg');
    svg.setAttribute('viewBox', '0 0 24 24'); svg.setAttribute('fill', 'none'); svg.setAttribute('stroke', 'currentColor');
    svg.setAttribute('stroke-width', '2'); svg.setAttribute('stroke-linecap', 'round'); svg.setAttribute('aria-hidden', 'true');
    var eye = document.createElementNS(NS, 'path'); eye.setAttribute('d', 'M2 12s3.6-7 10-7 10 7 10 7-3.6 7-10 7S2 12 2 12z');
    var pupil = document.createElementNS(NS, 'circle'); pupil.setAttribute('cx', '12'); pupil.setAttribute('cy', '12'); pupil.setAttribute('r', '3');
    var slash = document.createElementNS(NS, 'path'); slash.setAttribute('d', 'M4 4l16 16'); slash.setAttribute('class', 'slash');
    svg.appendChild(eye); svg.appendChild(pupil); svg.appendChild(slash);
    return svg;
  }
  Array.prototype.forEach.call(document.querySelectorAll('input[type=password]'), function (input) {
    var wrap = document.createElement('span');
    wrap.className = 'pw-wrap';
    input.parentNode.insertBefore(wrap, input);
    wrap.appendChild(input);
    var btn = document.createElement('button');
    btn.type = 'button'; btn.className = 'pw-toggle';
    btn.setAttribute('aria-label', 'Показать пароль'); btn.title = 'Показать пароль';
    btn.setAttribute('aria-pressed', 'false');
    btn.appendChild(eyeIcon());
    btn.addEventListener('click', function () {
      var show = input.type === 'password';
      input.type = show ? 'text' : 'password';
      btn.classList.toggle('on', show);
      btn.setAttribute('aria-pressed', String(show));
      var label = show ? 'Скрыть пароль' : 'Показать пароль';
      btn.setAttribute('aria-label', label); btn.title = label;
    });
    wrap.appendChild(btn);
    // перед отправкой формы поле снова становится скрытым: браузер не запомнит пароль как обычный текст
    if (input.form) input.form.addEventListener('submit', function () {
      if (input.type !== 'password') btn.click();
    });
  });

  // Кнопка «Вверх».
  var toTop = document.getElementById('to-top');
  if (toTop) {
    var syncTop = function () { toTop.classList.toggle('hidden', window.scrollY < 80); };
    window.addEventListener('scroll', syncTop, { passive: true });
    toTop.addEventListener('click', function () {
      var calm = window.matchMedia && window.matchMedia('(prefers-reduced-motion: reduce)').matches;
      window.scrollTo({ top: 0, behavior: calm ? 'auto' : 'smooth' });
    });
    syncTop();
  }

  // Чат: периодическое обновление ленты сообщений.
  var chat = document.getElementById('chat-list');
  if (chat) {
    chat.scrollTop = chat.scrollHeight;
    var lastChat = chat.innerHTML;
    var chatLoading = false;
    setInterval(function () {
      if (document.hidden || chatLoading) return;
      chatLoading = true;
      fetch(chat.getAttribute('data-src'), { credentials: 'same-origin' })
        .then(function (r) { return r.ok && !r.redirected && r.headers.get('X-Kafedra-Fragment') === 'chat' ? r.text() : null; })
        .then(function (html) {
          if (html === null || html === lastChat) return;
          var atBottom = chat.scrollHeight - chat.scrollTop - chat.clientHeight < 60;
          lastChat = html;
          chat.innerHTML = html;
          if (atBottom) chat.scrollTop = chat.scrollHeight;
        })
        .catch(function () {})
        .finally(function () { chatLoading = false; });
    }, 8000);
  }

  // Подтверждение необратимых действий.
  document.addEventListener('submit', function (ev) {
    var msg = ev.target.getAttribute('data-confirm');
    if (msg && !window.confirm(msg)) ev.preventDefault();
  });

  // Панель разметки редактора лекций.
  function wrap(area, before, after, placeholder) {
    var s = area.selectionStart, e = area.selectionEnd, v = area.value;
    var sel = v.slice(s, e) || placeholder;
    area.value = v.slice(0, s) + before + sel + after + v.slice(e);
    area.focus();
    area.selectionStart = s + before.length;
    area.selectionEnd = s + before.length + sel.length;
  }
  var marks = {
    bold: ['**', '**', 'текст'], italic: ['*', '*', 'текст'], h: ['\n## ', '\n', 'Заголовок'],
    ul: ['\n- ', '\n', 'пункт'], ol: ['\n1. ', '\n', 'пункт'], code: ['\n```\n', '\n```\n', 'код'],
    link: ['[', '](https://)', 'текст ссылки'], quote: ['\n> ', '\n', 'цитата'],
    table: ['\n| Столбец 1 | Столбец 2 |\n| --- | --- |\n| ', ' |  |\n', 'значение']
  };
  document.addEventListener('click', function (ev) {
    var b = ev.target.closest('[data-md]');
    if (b) {
      var area = document.getElementById(b.getAttribute('data-for') || 'body');
      var m = marks[b.getAttribute('data-md')];
      if (area && m) wrap(area, m[0], m[1], m[2]);
      return;
    }
    var ins = ev.target.closest('[data-insert]');
    if (ins) {
      var ta = document.getElementById('body');
      if (ta) wrap(ta, ins.getAttribute('data-insert'), '', '');
    }
  });

  // Показ полей формы вопроса в зависимости от типа.
  var qtype = document.getElementById('qtype');
  function syncType() {
    var text = qtype.value === 'text';
    document.getElementById('q-options').classList.toggle('hidden', text);
    document.getElementById('q-answers').classList.toggle('hidden', !text);
  }
  if (qtype) { qtype.addEventListener('change', syncType); syncType(); }

  // Тестирование: таймер, автосохранение, автоматическая отправка.
  var form = document.getElementById('attempt-form');
  if (form) {
    var saveUrl = form.getAttribute('data-save');
    var status = document.getElementById('save-status');
    var dirty = false;
    var saving = false;
    form.addEventListener('change', function () { dirty = true; });
    form.addEventListener('input', function () { dirty = true; });
    var save = function () {
      if (!dirty || saving) return;
      dirty = false;
      saving = true;
      fetch(saveUrl, { method: 'POST', body: new URLSearchParams(new FormData(form)), credentials: 'same-origin' })
        .then(function (r) {
          var saved = r.status === 204 && !r.redirected;
          if (!saved && r.status !== 409) dirty = true;
          if (status) status.textContent = saved ? 'Ответы сохранены ' + new Date().toLocaleTimeString() : 'Не удалось сохранить ответы';
        })
        .catch(function () { dirty = true; if (status) status.textContent = 'Нет связи с сервером — ответы будут сохранены при восстановлении'; })
        .finally(function () { saving = false; });
    };
    setInterval(save, 20000);
    var timer = document.getElementById('timer');
    if (timer) {
      var end = Date.now() + parseInt(timer.getAttribute('data-remaining'), 10) * 1000;
      var tick = function () {
        var left = Math.max(0, Math.round((end - Date.now()) / 1000));
        var m = Math.floor(left / 60), s = left % 60;
        timer.textContent = 'Осталось ' + m + ':' + (s < 10 ? '0' : '') + s;
        timer.classList.toggle('hot', left < 60);
        if (left <= 0) { clearInterval(iv); form.removeAttribute('data-confirm'); form.submit(); }
      };
      var iv = setInterval(tick, 500);
      tick();
    }
  }
})();
